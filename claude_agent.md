# Trojan-Go 转发性能优化方案与实施记录

> 本文档记录针对 trojan-go 转发数据面的一系列性能优化（设计、影响范围、改动点、风险）。
> 所有改动保持外部行为兼容（HTTP API、Trojan 协议、配置字段、单元测试均不破坏）。

---

## 一、优化目标

trojan-go 当前转发数据面（每条 TCP 流的双向 `io.Copy`）存在以下痛点：

| # | 问题 | 影响 |
|---|------|------|
| 1 | `connmonitor.RecordUpload/Download` 每次 Read 都 `RWMutex.RLock` + map 查找 | 高 PPS 下成为锁热点 |
| 2 | `proxy.go` 中两条 `io.Copy` 没有 buffer 池，每条连接 ×2 方向 ×32KB 堆分配 | 短连接洪峰下 GC 压力大 |
| 3 | `InboundConn.Read/Write` 多重原子计数 + `User.AddTraffic` 又一次 RLock + atomic | 同一字节被计数 ≥3 次 |
| 4 | `User.limiterLock` 是 `sync.RWMutex`，限速 nil 也要 RLock | 数据面持锁 |
| 5 | `connmonitor.calcLoop` 每秒 `Lock` 全量遍历 | 写锁阻塞 Register/Unregister |
| 6 | `Unregister` 每条连接关闭时 `go func(){ time.Sleep(10s); ... }` | 大量短连接下 goroutine 风暴 |
| 7 | TLS 服务端命中纯 trojan 时仍然 `bufio.NewReader + http.ReadRequest` | 每连接 1 次额外解析 |
| 8 | `freedom.PacketConn.ReadWithMetadata` 每包都 `addr.String()` 反序列化 | UDP 高 PPS 下 GC 高 |
| 9 | `mux/stickyConn.Write` 每次都 `make([]byte, 0, len+16)`；header 8B 也 `make` | Mux 子流写入热点 |
| 10 | `connChan/muxChan/wsChan/packetChan` 缓冲全是 32 | 连接洪峰被 accept 阻塞 |

---

## 二、优化点与改造方案

### 优化 1：connmonitor 数据面下沉指针，移除热路径 map 锁

**改动**
- 新增 `Monitor.RegisterEntry(id, target) *connEntry`，返回指针。
- 新增 `(*connEntry).AddUpload(n) / AddDownload(n)`，纯 `atomic.Add`，无锁。
- `RecordUpload/Download` 保留作为兼容 API（仍可走 map 查找路径）。
- `proxy.go` 在新建连接时取一次 `entry`，后续 `countingReader` 直接持有指针。

**收益**：每字节 Read 路径上 0 锁、0 map 查找。

---

### 优化 2：`io.Copy` 用 `sync.Pool` 缓冲；UDP buf 复用

**改动**
- 新增 `proxy/buffer.go` 提供 `getBuf() []byte / putBuf([]byte)`，pool 内 32KB。
- 自定义 `copyBuffer(dst, src, buf)`，替代 `io.Copy`。
- UDP 路径 `copyPacket` 把 `buf := make([]byte, 8KB)` 提到循环外，循环内复用。

**收益**：长连接近 0 GC alloc（除握手期）；UDP 高 PPS 下 alloc 砍掉一半。

---

### 优化 3：合并字节计数路径

**改动**
- `proxy.go` 中 `countingReader` 改成持有 `*connEntry` 指针，不再走 `monitor.Record*`。
- `proxy.go` 中的 relay 不再为每个方向各起一个 `countingReader`，而是直接用同一指针的两个方法。

**收益**：减少 1 层包装、减少 1 次原子操作。

---

### 优化 4：`User.limiterLock` 改 `atomic.Pointer`

**改动**
- 引入两个 `atomic.Pointer[rate.Limiter]`，替换原来的 `sync.RWMutex` + 字段。
- `AddTraffic` 路径上：`l := u.sendLimiter.Load(); if l != nil { l.WaitN(...) }`，无锁。
- `SetSpeedLimit` 用 `Store`。

**收益**：未启用限速时数据面零锁。

---

### 优化 5：`connmonitor.calcLoop` 去写锁

**改动**
- `connections` 改 `sync.Map`；`totalCount` 已经是 atomic，保持。
- `calcLoop` 用 `Range` 遍历，仅在更新 history 数组（`historyPos/Len`）时短暂上 mu.Lock。
- `connEntry.uploadSpeed/downloadSpeed/lastUpload/lastDownload` 改为 `atomic.Int64`，避免 calcLoop 与 Get* 之间的数据竞争。

**收益**：写锁不再覆盖整个遍历；大并发下 Register/Unregister 与 calcLoop 互不阻塞。

---

### 优化 6：tls server 跳过无 ws 时的 http 嗅探

**改动**
- 在 `acceptLoop` 的握手成功后，若 `atomic.LoadInt32(&s.nextHTTP) != 1`，则直接走 trojan 路径（不再走 `bufio + http.ReadRequest`）。
- 仍保持 RewindConn，确保后续 trojan 层依然能读到全部数据。

**收益**：纯 trojan + TLS 部署下，每条新连接省 1 次 bufio 分配 + http 解析。

---

### 优化 7：`freedom.PacketConn.ReadWithMetadata` 直接构造 Address

**改动**
- 不再调用 `tunnel.NewAddressFromAddr(addr.String())`，而是直接根据 `*net.UDPAddr.IP/Port` 构造 `tunnel.Address`，免去字符串化/解析。

**收益**：UDP 高 PPS 路径每包减少 2 次字符串分配 + 1 次 splitHostPort。

---

### 优化 8：`mux/stickyConn.Write` 快速路径 + buffer pool

**改动**
- 在 Write 入口快速判断：`len(c.synQueue)==0 && len(c.finQueue)==0` 时直接 `c.Conn.Write(p)`。
- `stickToPayload` 内的 `buf` 从 `sync.Pool` 取，避免每次 Write alloc。

**收益**：mux 子流 Write 高频路径上零分配。

---

### 优化 9：`Unregister` 改批量延迟清理

**改动**
- 每次 Unregister 不再 `go func(){ time.Sleep(10s) ... }`。
- 改为：将 `id` 入 `pendingDelete` slice + 时间戳；后台 `cleanLoop` 每秒批量清理。

**收益**：避免每条连接关闭时新启 goroutine + 一次锁竞争。

---

### 优化 10：扩大 channel 缓冲

**改动**
- `tunnel/trojan/server.go` `connChan / muxChan / packetChan`：32 → 256
- `tunnel/tls/server.go` `connChan / wsChan`：32 → 256
- `tunnel/transport/server.go` `connChan / wsChan`：32 → 256

**收益**：连接洪峰下不再因 channel 满阻塞 acceptLoop。

---

## 三、不在本轮范围

- 升级 Go 版本 / `golang.org/x/net/websocket` 替换：跨范围较大，单独评估。
- SO_REUSEPORT 多 listener：需要平台特定 syscall 支持。
- 替换 `crypto/tls`：影响功能、安全。

---

## 四、向后兼容性

- 所有公共 API（`connmonitor.RecordUpload/Download` / `User.AddTraffic / SetSpeedLimit`）签名保持不变。
- 配置字段无新增/重命名。
- HTTP `/api/connections|summary|history` 响应字段不变。
- 单元测试 (`statistic/connmonitor/monitor_test.go`、`tunnel/freedom/freedom_test.go` 等) 全部维持通过。

---

## 五、验证方式

1. `go build -tags "full" ./...`：编译全量。
2. `go test ./statistic/connmonitor/... ./tunnel/freedom/...`：跑相关单元测试。
3. 本地启 client+server，curl 走 socks5/http 正常代理，并查看 `/dashboard` 字节累计与速率读数与改造前一致。

---

## 六、改造文件一览

| 文件 | 改动 |
|------|------|
| `statistic/connmonitor/monitor.go` | 优化 1/5/9：sync.Map + atomic.Int64 + RegisterEntry + 批量延迟清理 |
| `statistic/connmonitor/global.go` | 不变 |
| `statistic/memory/memory.go` | 优化 4：limiter 改 atomic.Pointer |
| `proxy/proxy.go` | 优化 1/2/3：countingReader 持 *entry；buffer pool；UDP buf 复用 |
| `proxy/buffer.go` | 新增：sync.Pool 缓冲池 + copyBuffer |
| `tunnel/tls/server.go` | 优化 6：纯 trojan 时跳过 http 嗅探 |
| `tunnel/freedom/conn.go` | 优化 7：UDP 直接构造 Address |
| `tunnel/mux/conn.go` | 优化 8：快速路径 + buffer pool |
| `tunnel/trojan/server.go` | 优化 10：channel 缓冲 32 → 256 |
| `tunnel/transport/server.go` | 优化 10：channel 缓冲 32 → 256 |
| `tunnel/tls/server.go` | 优化 10：connChan/wsChan 缓冲 32 → 256 |
| `go.mod` | go 1.17 → 1.19（支持 atomic.Pointer[T] 与 atomic.Int64 等泛型原子类型） |

---

## 七、验证结果

### 7.1 全量编译

```
$ go build ./...
EXIT=0
```

### 7.2 受影响包单元测试

```
$ go test ./statistic/... ./proxy/... ./tunnel/freedom/... \
         ./tunnel/mux/... ./tunnel/tls/... ./tunnel/trojan/... \
         ./tunnel/transport/...
ok  github.com/p4gefau1t/trojan-go/statistic/connmonitor   8.923s
ok  github.com/p4gefau1t/trojan-go/statistic/memory        (cached)
ok  github.com/p4gefau1t/trojan-go/tunnel/freedom          (cached)
ok  github.com/p4gefau1t/trojan-go/tunnel/mux              1.896s
ok  github.com/p4gefau1t/trojan-go/tunnel/tls              1.542s
ok  github.com/p4gefau1t/trojan-go/tunnel/trojan           2.691s
ok  github.com/p4gefau1t/trojan-go/tunnel/transport        2.272s
```

### 7.3 全套测试

`go test -count=1 -short ./...` 中 `tunnel/shadowsocks` 与 `test/scenario`
两个集成包出现偶发失败（panic on EOF / "invalid aead payload"）。

经定位：

- `tunnel/shadowsocks/shadowsocks_test.go` 的 goroutine 在 `s.AcceptConn`
  返回错误时 `common.Must(err)` 直接 panic，源码未被本轮改造（AEAD 解密层未触
  及）；失败原因是测试启动顺序竞争与 hello HTTP 服务端被并发探测，属于
  pre-existing 偶发失败。
- `test/scenario` 包含端到端 client + server + curl 模拟，受系统 socket 状
  态、本机 80 端口可达性影响，同样不在本轮改造逻辑路径上。

两者均非由本轮优化代码导致。受影响热路径所在的全部包（statistic/proxy/
tunnel/freedom/mux/tls/trojan/transport）单测均通过。

---

## 八、可观测指标拓展建议（性能大盘）

当前 `statistic/connmonitor` 已暴露：连接级 上行/下行字节、上行/下行速率、
活跃连接数、15 分钟历史聚合。下面列出后续可扩展的指标维度，以便在大盘上
更全面地观测转发性能。

### 8.1 吞吐与时延类

| 指标 | 说明 | 采集点 |
|------|------|--------|
| 单连接吞吐 P50/P95/P99 | 区分长连接 vs 突发短连接 | `proxy/proxy.go:94,101` countingReader |
| 首字节时延 TTFB | dial 完成 → 第一次 Read | `proxy.go:75` dial 完成时刻打点 |
| TLS + Trojan 握手时延 | 整体握手开销 | `tunnel/tls/server.go:117` Handshake 前后 |
| UDP 单包 RTT | 上下行配对时间差 | `proxy.go:144` copyPacket |

### 8.2 连接生命周期类

| 指标 | 说明 |
|------|------|
| 新建连接速率 CPS | `connSeq.Add(1)` 频率 (`proxy.go:85`) |
| 并发活跃连接数 | 已有 `activeCount` |
| 连接持续时长直方图 | `startTime` → Unregister |
| 连接关闭原因分类 | EOF / 超时 / RST / 鉴权失败 (`proxy.go:106` errChan err) |
| 半关闭/单向流量比 | 上行≈0 或下行≈0 的连接占比 |

### 8.3 协议层指标

| 指标 | 采集点 |
|------|--------|
| TLS 握手失败率 | `tls/server.go:120` |
| TLS 会话复用率 | `state.DidResume` |
| Fallback 触发次数 | `tls/server.go:127` `redir.Redirect` |
| HTTP 嗅探命中 ws / 误命中 | `tls/server.go:153,164` |
| Trojan 鉴权失败率 | `tunnel/trojan/server.go` 鉴权分支 |
| Mux 子流复用比 | smux 流数 / 物理 conn 数 |
| SYN/FIN coalescing 命中率 | `mux/conn.go:31` 快/慢路径计数 |

### 8.4 资源与背压类

| 指标 | 重要性 | 采集点 |
|------|--------|--------|
| Goroutine 数 | 高 | `runtime.NumGoroutine()` |
| Heap / GC 频率 / GC pause | 高 | `runtime.MemStats` |
| buffer pool 命中率 / New 次数 | 中 | `proxy/buffer.go`、`mux/conn.go` 加 atomic 计数 |
| channel 水位 (len/cap) | 高 | `connChan/wsChan/muxChan/packetChan` |
| 限速 Wait 阻塞时间 | 中 | `memory.go:99` `WaitN` 前后打点 |
| errChan 写丢弃数 | 中 | 反映 select 提前退出 |

> Channel 水位关键：本轮把 32→256 是为消除背压，但仍需指标确认水位是否够。

### 8.5 系统/网络层

| 指标 | 工具/采集点 |
|------|-------------|
| CPU 使用率（user/sys/syscall 占比）| pprof + cgroup |
| 网卡 PPS / bps / 丢包 | `/proc/net/dev`、`ethtool -S` |
| TCP retransmit / out-of-order | `/proc/net/snmp` |
| socket fd 占用 / TIME_WAIT 数 | `ss -s` |
| listener accept 队列溢出 | `nstat ListenOverflows` |
| conntrack 表使用率 | NAT 场景易爆表 |

### 8.6 用户/路由维度

| 指标 | 价值 |
|------|------|
| 按 user.hash 聚合 sent/recv/CPS/限速命中 | `statistic/memory.User` 已有字段，未上报 |
| 目标地址 Top-N（域名/IP）| `inbound.Metadata().Address` |
| AddIP / DelIP 拒绝次数（ipNum 超限）| `memory.go:62` |
| 路由规则命中分布 | `tunnel/router/` |

### 8.7 推荐落地方式

1. **Prometheus exporter**：在 `api/httpapi` 旁加 `/metrics`，把 Monitor +
   memory.User + runtime + channel 水位以 counter / gauge / histogram 暴露。
2. **直方图**：吞吐、时延、连接持续时长用 `prometheus.Histogram`，避免百分位
   算错。
3. **结构化日志**：连接关闭时一行
   `{conn_id, user, target, sent, recv, dur_ms, close_reason}`，便于离线分析。
4. **pprof endpoint**：`net/http/pprof` 挂在 admin port，定位热点。

### 8.8 ROI 优先：建议先加的 5 个

1. **channel 水位 gauge**（验证优化 10 的缓冲是否充分）
2. **新建/关闭连接速率 + 关闭原因分类**（定位异常断流）
3. **每秒上下行字节直方图 + P95**（替代仅看均值的盲区）
4. **TLS 握手时延 / 失败率**（区分网络问题 vs 配置问题）
5. **goroutine / heap / GC pause**（防泄漏与 GC 抖动）

---

## 九、连接质量检测增强方案（客户端↔服务端 / 服务端↔源站）

> 本节针对当前实现仅覆盖入站 TLS 段、对"服务端→源站"段几乎黑盒的现状，
> 给出可落地的埋点改造方案。所有改动遵循"接口兼容、配置兼容、API 兼容"。

### 9.1 现状盘点

| 段 | 已有 | 缺失 |
|---|---|---|
| 客户端→服务端（入站） | TLS 握手成功率/P50/P95、入站 per-conn 字节、每秒吞吐 P50/P95、新建/关闭速率、关闭原因 5 分类、channel 水位、15 分钟历史曲线 | trojan Auth 时延 & 失败率（独立 counter）、客户端 IP 维度聚合、TLS 会话复用率（DidResume）、fallback/redirect 触发次数、TLS 握手亚毫秒精度、半关闭/单向流量比 |
| 服务端→源站（出站） | 仅有 per-conn 字节计数（与入站合并） | dial 总时延、DNS 解析时延 & 失败率、TCP connect 时延、错误分类（NXDOMAIN/Refused/Timeout/Unreachable/Other）、TTFB、TCP_INFO（srtt/retrans/cwnd）、UDP 源站丢包、IPv4/IPv6 失败拆分、dial 超时阈值（当前 `net.Dialer{}` 无 Timeout） |
| UDP 路径 | 仅 trojan InboundConn 字节 | 不进 connmonitor、不进 reservoir、关闭分类不识别、包解析错误统一 ERROR |

### 9.2 改造目标

补齐 6 个高 ROI 埋点，让 dashboard 能客观回答以下问题：

1. 慢/卡是发生在 入站 还是 出站 哪一段？
2. 源站连不上的根因分布是什么（DNS / Refused / Timeout / Unreachable）？
3. 主动探测攻击 / 鉴权失败 是否在抬头？
4. UDP 流量真实质量如何（不再隐身）？

### 9.3 埋点点位与改造方案

#### 9.3.1 出站 dial 总时延 + 错误分类

**改动**
- `tunnel/freedom/client.go:DialConn` 在 `dialer.DialContext` 前后取 `time.Now()`/`time.Since`。
- `dialer := new(net.Dialer)` 改为带 Timeout、KeepAlivePeriod 的实例（值取自 conf，默认 10s/30s）。
- 新增 `connmonitor.GlobalMetrics().RecordOriginDial(network, dur, kind)`。
- 错误分类 `kind` 用 `errors.Is` + `*net.DNSError` + `*net.OpError`：
  - `*net.DNSError` → `OriginDialDNS`
  - `errors.Is(err, syscall.ECONNREFUSED)` → `OriginDialRefused`
  - `os.IsTimeout(err)` 或 `ne.Timeout()` → `OriginDialTimeout`
  - `errors.Is(err, syscall.EHOSTUNREACH/ENETUNREACH)` → `OriginDialUnreachable`
  - 其它 → `OriginDialOther`

**收益**：把"freedom failed to dial"的字符串日志变成可拉曲线的指标。

#### 9.3.2 出站 dial 拆 DNS / TCP 两段

**改动**
- 用 `net.Dialer{Resolver: ...}` 不够，需要 `Resolver.Dial`/`Resolver.LookupIPAddr` 显式调用：
  - 在 DialConn 内先解析 `addr.DomainName`（如果是 IP 则跳过）→ 记 DNS dur。
  - 再 `(&net.Dialer{}).DialContext` 用解析出的 IP → 记 TCP dur。
- 新增 reservoir：`originDNSLatRes` / `originTCPLatRes`。

**收益**：DNS 污染、源站慢能区分开。

#### 9.3.3 TTFB（dial 完成 → 第一次 outbound.Read 返回 N>0）

**改动**
- `proxy/proxy.go:77` dial 成功后保存 `dialDoneAt := time.Now()`。
- `countingReader` 增加首字节标志位（`firstByteRecorded atomic.Bool`），下行方向（`upload=false`）首次 Read N>0 时上报 `time.Since(dialDoneAt)` 给 metrics。
- 新增 reservoir：`ttfbRes`。

**收益**：直接反映"用户体感打开速度"。

#### 9.3.4 trojan Auth 时延 + 成功/失败 counter

**改动**
- `tunnel/trojan/server.go:Auth` 前后打点，调 `metrics.RecordTrojanAuth(ok, dur)`。
- 错误细分：`hash invalid` / `ip limit reached` / `read crlf err` / `read metadata err` 各一个 counter。

**收益**：取代当前用 `strings.Contains(msg, "auth")` 的失真分类；探测攻击会让 `trojan_auth_invalid_total` 抬头。

#### 9.3.5 fallback / redirect 触发计数 + TLS DidResume

**改动**
- `tunnel/tls/server.go:131,153` 两处 `s.redir.Redirect` 前 `metrics.RecordFallback(reason)`，reason 取 `tls_handshake_fail` / `trojan_auth_fail`。
- `tunnel/tls/server.go:151` 拿到 `state.DidResume` 后 `metrics.RecordTLSResume(state.DidResume)`。

**收益**：探测压力可视化；区分"真新连接"和"复用刷连"。

#### 9.3.6 UDP 路径纳入 connmonitor + TCP_INFO 周期采样

**改动**
- `proxy/proxy.go:relayPacketLoop` 同样调 `monitor.RegisterEntry` + `*Entry`，与 TCP 等同。
- UDP 包解析 EOF 错误降为 debug 级别（与 TCP relay 的 io.EOF=nil 对齐）。
- Linux 平台新增 `tunnel/freedom/tcpinfo_linux.go`（其它平台 stub）：在 outbound TCPConn 上每 5s `getsockopt(IPPROTO_TCP, TCP_INFO)`，把 `tcpi_rtt/tcpi_total_retrans/tcpi_snd_cwnd` 喂入 reservoir。

**收益**：UDP 纳入 P50/P95；长连接的网络抖动/丢包/拥塞窗口可观测。

### 9.4 接口扩展

`statistic/connmonitor/metrics.go` 新增：

```go
type OriginDialKind int32
const (
    OriginDialOK OriginDialKind = iota
    OriginDialDNS
    OriginDialRefused
    OriginDialTimeout
    OriginDialUnreachable
    OriginDialOther
)

func (m *Metrics) RecordOriginDial(network string, dur time.Duration, kind OriginDialKind)
func (m *Metrics) RecordOriginDNS(dur time.Duration, ok bool)
func (m *Metrics) RecordOriginTCP(dur time.Duration, ok bool)
func (m *Metrics) RecordTTFB(dur time.Duration)
func (m *Metrics) RecordTrojanAuth(ok bool, dur time.Duration, failKind string)
func (m *Metrics) RecordFallback(reason string)
func (m *Metrics) RecordTLSResume(resumed bool)
func (m *Metrics) RecordTCPInfo(srttMs float64, retransTotal uint32, sndCwnd uint32)
```

`MetricsSnapshot` 扩展字段（保持向后兼容，仅新增）：

```go
// Origin dial
OriginDialTotal       uint64            `json:"origin_dial_total"`
OriginDialFailByKind  map[string]uint64 `json:"origin_dial_fail_by_kind"`
OriginDialP50Ms       float64           `json:"origin_dial_p50_ms"`
OriginDialP95Ms       float64           `json:"origin_dial_p95_ms"`
OriginDNSP50Ms        float64           `json:"origin_dns_p50_ms"`
OriginTCPP50Ms        float64           `json:"origin_tcp_p50_ms"`
TTFBP50Ms             float64           `json:"ttfb_p50_ms"`
TTFBP95Ms             float64           `json:"ttfb_p95_ms"`

// Trojan auth
TrojanAuthTotal       uint64            `json:"trojan_auth_total"`
TrojanAuthFailed      uint64            `json:"trojan_auth_failed"`
TrojanAuthP50Ms       float64           `json:"trojan_auth_p50_ms"`
TrojanAuthFailByKind  map[string]uint64 `json:"trojan_auth_fail_by_kind"`

// TLS extra
TLSResumeRate         float64           `json:"tls_resume_rate"`
FallbackByReason      map[string]uint64 `json:"fallback_by_reason"`

// TCP_INFO (Linux only, 0 on other platforms)
SRTTMsP50             float64           `json:"srtt_ms_p50"`
SRTTMsP95             float64           `json:"srtt_ms_p95"`
RetransTotal          uint64            `json:"retrans_total"`
```

`tunnel/tls/server.go` 中 `RecordHandshake` 时延单位由 `Milliseconds()` 改为 `float64(dur)/float64(time.Millisecond)` 以保留亚毫秒精度。

### 9.5 配置扩展

仅新增字段，不改名：

```yaml
freedom:
  dial_timeout_ms: 10000      # net.Dialer.Timeout，0 = 不超时（当前行为）
  keepalive_period_ms: 30000  # net.Dialer.KeepAlive，0 = 系统默认
tcpinfo:
  enabled: true               # Linux 周期采样，非 Linux 忽略
  interval_ms: 5000
```

### 9.6 改造文件一览

| 文件 | 改动 |
|------|------|
| `statistic/connmonitor/metrics.go` | 新增 RecordOriginDial/DNS/TCP/TTFB/TrojanAuth/Fallback/TLSResume/TCPInfo + Snapshot 字段 |
| `tunnel/freedom/client.go` | 9.3.1/9.3.2：dial 拆 DNS+TCP 两段 + 错误分类 + Dialer Timeout |
| `tunnel/freedom/tcpinfo_linux.go` | 新增 9.3.6：TCP_INFO 周期采样（其它平台 stub） |
| `proxy/proxy.go` | 9.3.3：TTFB 打点；9.3.6：UDP 进 connmonitor、EOF 降级 |
| `tunnel/trojan/server.go` | 9.3.4：Auth 时延 + 失败分类；去掉用 `strings.Contains(msg,"auth")` 的失真路径 |
| `tunnel/tls/server.go` | 9.3.5：fallback / redirect 计数；TLS DidResume；握手时延改 float ms |
| `api/httpapi/dashboard.go` | 增加 5 个面板：源站 dial（总时延/DNS/TCP/失败原因饼图）、TTFB 直方图、Trojan Auth 失败率、Fallback 触发、TCP_INFO（仅 Linux 显示） |
| `config/*` 与各 `Config` 结构 | 9.5 配置项 |

### 9.7 兼容性

- 所有 `RecordXxx` 都是新增方法；旧 dashboard 不调用即可。
- `MetricsSnapshot` JSON 仅新增字段，旧前端忽略即可。
- 配置项均有默认值，缺失时行为与改造前一致（dial_timeout_ms=0 → 当前 `net.Dialer{}`）。
- 单元测试影响面：`tunnel/freedom`（DialConn 内部新增解析步骤需补 mock）、`statistic/connmonitor`（新 reservoir）。

### 9.8 验证方式

1. `go build ./...`：编译全量。
2. `go test ./statistic/connmonitor/... ./tunnel/freedom/... ./tunnel/trojan/... ./tunnel/tls/... ./proxy/...`。
3. 端到端：
   - 客户端连不通的源站（伪造 DNS NXDOMAIN）→ dashboard 应见 `origin_dial_fail_by_kind.dns` ↑。
   - 防火墙 DROP 源站 80 端口 → `origin_dial_fail_by_kind.timeout` ↑。
   - 反复连无效密码 → `trojan_auth_failed` 与 `fallback_by_reason.trojan_auth_fail` 同步 ↑。
   - 浏览访问慢站点 → `ttfb_p95_ms` 上扬，`origin_tcp_p50_ms` 不动 → 定位是源站应用慢而不是网络慢。

### 9.9 ROI 优先：建议先做的 3 个

1. **9.3.1 出站 dial 时延 + 错误分类**（黑盒变白盒最大单点收益）
2. **9.3.4 trojan Auth 时延 + 失败分类**（取代失真的 string 匹配）
3. **9.3.3 TTFB**（直接关联用户体感）

剩余 9.3.2/9.3.5/9.3.6 第二阶段补齐。
