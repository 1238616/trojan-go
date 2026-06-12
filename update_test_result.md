# 集群出口优选 — 测试结果

**测试日期**：2026-06-04  
**Go 版本**：go 1.24  
**平台**：darwin/arm64 (Apple M4 Pro)  
**测试命令**：`go test -v -race -count=1 ./cluster/` + `go test -v -race -count=1 ./proxy/`

---

## 一、总体结果

| 类别 | 测试数 | 通过 | 失败 | 跳过 |
|------|--------|------|------|------|
| cluster 单元测试 | 40 | 40 | 0 | 0 |
| proxy 回归测试 | 11 | 11 | 0 | 0 |
| **合计** | **51** | **51** | **0** | **0** |

- **Race Detector**：无数据竞争检出
- **go vet**：无静态分析警告
- **go build -tags full**：编译成功

---

## 二、单元测试详细结果

### 2.1 cluster/config_test.go（2/2 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestConfigDefaults | ✅ PASS | 0.00s | ProbeInterval=120, ProbeTimeout=3000, RelayThreshold=50, LatencyThreshold=100, NodeName="local" |
| TestConfigParsePeers | ✅ PASS | 0.00s | JSON 解析 peers + websocket/ssl 子字段正确 |

### 2.2 cluster/matcher_test.go（6/6 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestMatcherCIDR | ✅ PASS | 0.00s | 149.154.175.53 匹配 cidr:149.154.160.0/20，8.8.8.8 不匹配 |
| TestMatcherDomain | ✅ PASS | 0.08s | telegram.org 精确匹配，api.telegram.org 后缀匹配，example.com 不匹配 |
| TestMatcherIP | ✅ PASS | 0.00s | ip:149.154.175.53 精确匹配 |
| TestMatcherEmpty | ✅ PASS | 0.00s | 空规则列表不匹配任何地址 |
| TestMatcherMultiRules | ✅ PASS | 0.00s | 5 个子用例：CIDR(149.154/91.108)、domain、ip、no-match |
| TestMatcherNil | ✅ PASS | 0.00s | nil matcher 不匹配 |

### 2.3 cluster/route_table_test.go（8/8 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestRouteTableUpdate | ✅ PASS | 0.00s | Update 后 GetAllEntries 包含正确 peer 记录 |
| TestRouteTableEWMA | ✅ PASS | 0.00s | 两次 Update(100ms, 200ms) → EWMA=130ms (alpha=0.3) |
| TestRouteTableBestExitBasic | ✅ PASS | 0.00s | local=250ms, la-1=15ms → BestExit 返回 la-1，gain=235ms |
| TestRouteTableBestExitBelowThreshold | ✅ PASS | 0.00s | local=80ms, peer=60ms, threshold=50ms → 差值 20ms < 50ms → 返回 "" |
| TestRouteTableBestExitUnavailable | ✅ PASS | 0.00s | peer RTT=-1 → 不可用 → 返回 "" |
| TestRouteTableStaleEntry | ✅ PASS | 0.00s | UpdatedAt 15分钟前 → 该记录被忽略 |
| TestRouteTableMultiplePeers | ✅ PASS | 0.00s | 4 个 peer 中选延迟最低的 peer-b(50ms) |
| TestRouteTableNoLocalEntry | ✅ PASS | 0.00s | 无 local 基线 → 返回 "" |
| TestRouteTableGetAllEntries | ✅ PASS | 0.00s | 返回的是拷贝，修改不影响原始数据 |

### 2.4 cluster/metrics_test.go（5/5 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestMetricsRecordRelay | ✅ PASS | 0.00s | 3 次 RecordRelay → TotalRelays=3, AvgGainMs=143.3 |
| TestMetricsRecordFallback | ✅ PASS | 0.00s | 3 次 RecordRelayFallback → TotalFallbacks=3 |
| TestMetricsRecordProbe | ✅ PASS | 0.00s | RecordProbe(312ms) → LastProbeDurMs≈312 |
| TestMetricsConcurrency | ✅ PASS | 0.00s | 100 goroutine × 3 并发操作，race detector 无报警 |
| TestMetricsZeroRelaysAvgGain | ✅ PASS | 0.00s | 零中继时 AvgGainMs=0 |

### 2.5 cluster/peer_dialer_test.go（6/6 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestHexSHA224 | ✅ PASS | 0.00s | SHA-224("password") = d63dc919e201d7bc... 与已知值一致 |
| TestTrojanHeaderFormatIPv4 | ✅ PASS | 0.00s | 格式：56字节hex + CRLF + CMD(0x01) + ATYP(0x01) + IP(4bytes) + Port(2bytes) + CRLF |
| TestTrojanHeaderFormatDomain | ✅ PASS | 0.00s | ATYP=0x03 + domainLen + domain 字符串正确 |
| TestPeerDialerCreateWithWebsocket | ✅ PASS | 0.00s | wsEnable=true, wsHost/wsPath/sni/verify 正确填充 |
| TestPeerDialerCreateWithoutWebsocket | ✅ PASS | 0.00s | wsEnable=false, sni 默认回退到 host |
| TestPeerDialerDefaultWSPath | ✅ PASS | 0.00s | ws enabled 但 path/host 为空 → 默认 "/" 和 host |

### 2.6 cluster/prober_test.go（8/8 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestProberIntervalClamp | ✅ PASS | 0.00s | ProbeInterval=30 → 被 clamp 到 60s |
| TestProberRegisterSlowTarget | ✅ PASS | 0.00s | dialRTT=250ms > threshold=100ms → 注册成功 |
| TestProberRegisterBelowThreshold | ✅ PASS | 0.00s | dialRTT=50ms < threshold=100ms → 不注册 |
| TestProberEvictStaleTargets | ✅ PASS | 0.00s | 31分钟前的 target 被淘汰，当前的保留 |
| TestProberDynamicTargetRefresh | ✅ PASS | 0.01s | 再次注册同一 target → LastDialRTT 更新为新值 |
| TestProbeDirectTCP | ✅ PASS | 0.00s | 本地 TCP listener → RTT > 0 且 < 100ms |
| TestProbeDirectTimeout | ✅ PASS | 0.50s | 不可达目标(192.0.2.1:1) → 返回 -1 |
| TestProberStaticTargetParsing | ✅ PASS | 0.00s | 裸 IP 解析为 port=443 的探测目标 |
| TestProberHostPortTarget | ✅ PASS | 0.00s | "127.0.0.1:8080" 解析为 host+port |

### 2.7 cluster/router_test.go（6/6 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestRouterDialConnDisabled | ✅ PASS | 0.00s | enabled=false → (nil, "local", nil) |
| TestRouterDialConnNil | ✅ PASS | 0.00s | nil router → (nil, "local", nil) |
| TestRouterDialConnNotInTargets | ✅ PASS | 0.00s | 8.8.8.8 不在 cidr:149.154.160.0/20 → local |
| TestRouterDialConnLocalOptimal | ✅ PASS | 0.00s | local=30ms, peer=25ms, gain=5ms < threshold=50ms → local |
| TestRouterDialConnRelayNoPeerDialer | ✅ PASS | 0.00s | 路由表指向 la-1 但无 dialer → fallback local |
| TestRouterSnapshotStructure | ✅ PASS | 0.00s | Snapshot 包含 Enabled/LocalNode/Peers/OptimizedRoutes/Stats |

---

## 三、proxy 回归测试（11/11 PASS）

| 用例 | 结果 | 耗时 | 说明 |
|------|------|------|------|
| TestSetRelayBufferSizeClamp | ✅ PASS | 0.00s | buffer 大小 clamp 逻辑无回归 |
| TestSetRelayBufferSizeAlignment | ✅ PASS | 0.00s | 对齐逻辑无回归 |
| TestPhase4ConfigFields | ✅ PASS | 0.00s | GOGC/MemLimitMB 配置解析无回归 |
| TestPhase4ConfigZeroDefaults | ✅ PASS | 0.00s | 零值默认无回归 |
| TestDebugConfigFromJSON | ✅ PASS | 0.00s | debug 字段解析无回归 |
| TestProfilerCreatesFiles | ✅ PASS | 0.00s | profiler 文件输出无回归 |
| TestProfilerGoroutineDump | ✅ PASS | 0.00s | goroutine dump 无回归 |
| TestProfilerStopCancels | ✅ PASS | 0.20s | Stop → context 取消无回归 |
| TestProfilerRotateFiles | ✅ PASS | 0.01s | 日志轮转无回归 |
| TestProfilerCleanOldArchives | ✅ PASS | 0.00s | 旧归档清理无回归 |
| TestProfilerCheckRotate | ✅ PASS | 0.00s | 日期切换检查无回归 |

**结论**：cluster 集成不影响 proxy 包现有功能。

---

## 四、性能基准测试

```
goos: darwin
goarch: arm64
cpu: Apple M4 Pro
```

| Benchmark | 次数 | ns/op | B/op | allocs/op |
|-----------|------|-------|------|-----------|
| BenchmarkMetricsRecordRelay | 184,792,906 | 6.315 | 0 | 0 |
| BenchmarkRouteTableBestExit | 16,339,544 | 73.05 | 0 | 0 |
| BenchmarkMatcherMatch | 120,307,017 | 10.00 | 0 | 0 |

### 性能分析

- **MetricsRecordRelay（6.3 ns/op, 0 alloc）**：使用 atomic.AddInt64 实现，无锁竞争，每秒可处理约 1.6 亿次中继记录。对数据路径零影响。

- **RouteTableBestExit（73 ns/op, 0 alloc）**：每次查询仅需 RLock + 线性扫描。以 4 个 peer 为例，每秒可执行约 1370 万次路由查询。实际场景中每个新连接仅查询一次，性能远超需求。

- **MatcherMatch（10 ns/op, 0 alloc）**：CIDR 匹配基于 net.IPNet.Contains()，O(n) 遍历规则集。4 条规则下 10ns 的开销对连接建立的 ms 级耗时可忽略不计。

### 热路径影响评估

集群路由决策发生在连接建立阶段（`relayConnLoop` 的 `DialConn` 之前），每个连接仅执行一次：
1. `MatcherMatch`：~10ns
2. `BestExit` 查询：~73ns
3. 总开销：~83ns per connection

对比连接建立的 TCP 握手 + TLS 握手（ms 级），集群路由的 CPU 开销可忽略不计（< 0.01%）。

---

## 五、构建验证

| 检查项 | 结果 |
|--------|------|
| `go build -tags full ./...` | ✅ 编译成功 |
| `go vet ./cluster/ ./proxy/` | ✅ 无警告 |
| `go test -race ./cluster/` | ✅ 无数据竞争 |
| `go test -race ./proxy/` | ✅ 无数据竞争 |

---

## 六、测试覆盖的改动文件

| 文件 | 测试文件 | 覆盖用例数 |
|------|----------|-----------|
| cluster/config.go | cluster/config_test.go | 2 |
| cluster/matcher.go | cluster/matcher_test.go | 6 |
| cluster/route_table.go | cluster/route_table_test.go | 8 + 1 benchmark |
| cluster/metrics.go | cluster/metrics_test.go | 5 + 1 benchmark |
| cluster/peer_dialer.go | cluster/peer_dialer_test.go | 6 |
| cluster/prober.go | cluster/prober_test.go | 8 |
| cluster/router.go | cluster/router_test.go | 6 + 1 benchmark |
| proxy/proxy.go (集成) | proxy/*_test.go | 11 (回归) |

---

## 七、未覆盖的测试项（需网络/多节点环境）

以下测试项在测试计划中列出，但需要完整的 trojan-go server 实例运行，无法在纯单元测试中执行：

| 测试项 | 原因 | 替代验证 |
|--------|------|----------|
| TestPeerDialerWebsocketHandshake | 需运行中的 trojan-go server (ws 模式) | TrojanHeader 格式测试 + PeerDialer 配置测试覆盖了协议层 |
| TestPeerDialerTLSVerify | 需自签名证书 server | verify 字段解析已测试 |
| TestPeerDialerWebsocketPathMismatch | 需 ws server | ws 配置传递已测试 |
| TestProbeViaPeerRelay | 需 2 个 trojan server + HTTP target | probeDirect 已验证探测逻辑 |
| 多节点端到端中继 (5.1-5.3) | 需 docker-compose 或多机部署 | 路由决策逻辑已通过单元测试覆盖 |
| HTTP API /api/cluster (7.1-7.2) | 需完整 proxy 启动 | Snapshot 结构已测试 |

**建议**：上述测试应在 CI/CD 环境中通过 docker-compose 搭建多节点环境执行。

---

## 八、结论

1. **功能正确性**：40 个 cluster 单元测试全部通过，覆盖配置解析、目标匹配、路由决策、EWMA 平滑、探测注册/淘汰、协议头格式、指标并发安全等核心功能。

2. **无回归**：11 个 proxy 回归测试全部通过，cluster 集成不影响现有 profiler、buffer、config 功能。

3. **无数据竞争**：所有测试在 `-race` 模式下通过，包含 100 并发 goroutine 的压力测试。

4. **性能无影响**：热路径 CPU 开销 < 100ns/连接（MatcherMatch + BestExit），全部零内存分配。MetricsRecordRelay 6.3ns/op，不会成为瓶颈。

5. **构建完整**：`go build -tags full` 编译成功，`go vet` 无警告。

---

## 九、sing-mux 兼容性测试结果

**测试日期**：2026-06-05
**测试环境**：macOS Darwin 25.3.0, Go 1.19, xtaci/smux v1.5.15, hashicorp/yamux v0.1.2

### 9.1 编译验证

| 测试项 | 结果 | 备注 |
|--------|------|------|
| `go build -tags full ./...` | PASS | 所有包编译通过 |
| Linux 交叉编译 | PASS | `trojan-go-linux-amd64` 16MB |

### 9.2 协议编解码单元测试

```
=== RUN   TestReadRequest_V0Smux        --- PASS
=== RUN   TestReadRequest_V0Yamux       --- PASS
=== RUN   TestReadRequest_V1NoPadding   --- PASS
=== RUN   TestReadRequest_V1WithPadding --- PASS
=== RUN   TestReadRequest_InvalidVersion--- PASS
=== RUN   TestReadRequest_Empty         --- PASS
=== RUN   TestReadStreamRequest_TCPIPv4 --- PASS
=== RUN   TestReadStreamRequest_TCPDomain--- PASS
=== RUN   TestReadStreamRequest_TCPIPv6 --- PASS
=== RUN   TestReadStreamRequest_UDP     --- PASS
```

**结果**：10/10 PASS

| 用例 | 验证点 | 结果 |
|------|--------|------|
| V0 smux | Version=0, Protocol=0, Padding=false | PASS |
| V0 yamux | Protocol=1 | PASS |
| V1 无 padding | Version=1, Padding=false | PASS |
| V1 有 padding（100 字节）| 正确跳过 padding 数据 | PASS |
| 非法版本 (0x02) | 返回 error | PASS |
| 空输入 | 返回 io.EOF | PASS |
| TCP + IPv4 (1.2.3.4:443) | Network=tcp, IP 和 Port 正确 | PASS |
| TCP + Domain (google.com:80) | DomainName 和 Port 正确 | PASS |
| TCP + IPv6 (2001:db8::1:8080) | IPv6 地址和 Port 正确 | PASS |
| UDP (8.8.8.8:53) | Network=udp | PASS |

### 9.3 responseStream 测试

```
=== RUN   TestResponseStream_FirstWrite          --- PASS
=== RUN   TestResponseStream_SecondWrite         --- PASS
=== RUN   TestResponseStream_ConcurrentFirstWrite--- PASS
```

**结果**：3/3 PASS

| 用例 | 验证点 | 结果 |
|------|--------|------|
| 首次 Write "hello" | 底层收到 `[0x00, h, e, l, l, o]`，n=5 | PASS |
| 第二次 Write "world" | 底层收到 `[w, o, r, l, d]`（无 status） | PASS |
| 10 goroutine 并发首次 Write | status byte 仅出现 1 次 | PASS |

### 9.4 session 创建测试

```
=== RUN   TestNewServerSession_H2MuxUnsupported  --- PASS
=== RUN   TestNewServerSession_UnknownProtocol   --- PASS
```

**结果**：2/2 PASS

| 用例 | 验证点 | 结果 |
|------|--------|------|
| h2mux 协议 | 返回 "not yet supported" 错误 | PASS |
| 未知协议 (99) | 返回 error | PASS |

### 9.5 ProtocolName 测试

```
=== RUN   TestProtocolName --- PASS
```

| 输入 | 预期输出 | 结果 |
|------|----------|------|
| 0 (smux) | "smux" | PASS |
| 1 (yamux) | "yamux" | PASS |
| 2 (h2mux) | "h2mux" | PASS |
| 99 | "unknown(99)" | PASS |

### 9.6 Race 检测

```
go test -v -race ./tunnel/singmux/
PASS ok github.com/p4gefau1t/trojan-go/tunnel/singmux 1.518s
```

**结果**：16/16 PASS，无数据竞争

### 9.7 兼容性回归

| 测试套件 | 结果 | 耗时 |
|----------|------|------|
| `./tunnel/mux/` | PASS | 2.946s |
| `./tunnel/trojan/` | PASS | 2.869s |

现有 mux 和 trojan 包的测试全部通过，sing-mux 变更不影响原有功能。

### 9.8 待部署后测试

以下测试项需要实机环境，部署新版 trojan-go 后执行：

| 测试项 | 状态 | 说明 |
|--------|------|------|
| sing-box 客户端端到端（smux） | PENDING | 需部署到服务器 |
| sing-box 客户端端到端（yamux） | PENDING | 需部署到服务器 |
| Shadowrocket 实机测试 | PENDING | 需 iOS 设备 |
| 确认 Shadowrocket mux 协议类型 | PENDING | 查看 `singmux: new session protocol=` 日志 |
| 长连接稳定性（24h） | PENDING | 需部署运行 |
| 与 cluster routing 共存 | PENDING | 需部署运行 |

### 9.9 构建产物

| 目标 | 文件 | 大小 |
|------|------|------|
| linux/amd64 | `trojan-go-linux-amd64` | 16 MB |

### 9.10 结论

1. **协议编解码正确**：sing-mux Request Header（V0/V1, 含 padding）、StreamRequest（TCP/UDP, IPv4/Domain/IPv6）、StreamResponse（status byte）全部正确解析。

2. **并发安全**：responseStream 在 10 goroutine 并发写入下仅发送 1 次 status byte，race detector 无告警。

3. **无回归**：现有 mux 和 trojan 测试套件全部通过，sing-mux 路由分发不影响 MUX_CONN 和普通连接路径。

4. **构建成功**：`go build -tags full` 编译通过，Linux 交叉编译成功产出 16MB 二进制。

5. **待验证**：sing-box 客户端端到端测试和 Shadowrocket 实机测试需要部署到服务器后执行，特别是确认 Shadowrocket 使用的 mux 协议类型（smux/yamux/h2mux）。
