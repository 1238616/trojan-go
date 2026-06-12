# Trojan-Go 性能优化与Debug模式 — 更新计划

## 构建要求

**重要**: 构建时必须使用 `-tags full`，否则 client/server/custom/forward 等模块不会被编译进二进制。

```bash
# 推荐方式：使用 Makefile（已内置 -tags full）
make linux-amd64

# 手动构建（必须加 -tags full）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -tags full \
  -trimpath -ldflags="-s -w" \
  -o trojan-go-linux-amd64 .

# 验证：正确构建的完整版约 15MB，缺少 -tags full 的只有约 3.4MB
```

---

## 一、P0 问题修复

### P0-1: RewindReader 互斥锁热路径优化

**文件**: `common/io.go`

**问题**: `RewindReader.Read()` 在每次调用时都会获取 `sync.Mutex`，即使在 `StopBuffering()` 之后已经不再需要缓冲。对于已建立的连接，这是纯开销——每个连接的每次 Read 都会经历一次 lock/unlock。

**修复方案**:
- 新增 `passthrough atomic.Bool` 字段
- `StopBuffering()` 和 `SetBufferSize(0)` 在确认无 rewind 待处理时设置 `passthrough = true`
- `Read()` 开头检查 `passthrough`，若为 true 则直接调用 `rawReader.Read()` 跳过互斥锁
- `Rewind()` 将 `passthrough` 重置为 false，确保安全回退到锁保护路径
- `SetBufferSize(n>0)` 重新启用缓冲时也重置 `passthrough`

**性能影响**: 稳态下每次 Read 从 mutex lock/unlock 降为单次 atomic.Bool.Load()（约 1ns vs 约 25ns）

### P0-2: Trojan Client 100ms 定时器延迟

**文件**: `tunnel/trojan/client.go`

**问题**: `DialConn()` 中为每个新连接启动一个 goroutine，sleep 100ms 后发送 header。这意味着：
1. 如果服务端在等待 header 才回数据（如 HTTP 请求），客户端无条件等 100ms
2. 每个连接产生一个短生命周期 goroutine
3. 高并发下产生大量 goroutine 调度开销

**修复方案**:
- 删除 timer goroutine
- 新增 `headerFlushed atomic.Bool` 字段到 `OutboundConn`
- `WriteHeader()` 成功后设置 `headerFlushed = true`
- `Read()` 中检查 `headerFlushed`，若为 false 则先调用 `WriteHeader(nil)` 发送 header
- 效果：header 在首次 Write 或首次 Read 时按需发送，零延迟

**性能影响**: 消除 100ms 固定延迟，减少每连接 1 个 goroutine

### P0-3: Mux Server Channel 容量

**文件**: `tunnel/mux/server.go`

**问题**: `connChan` 容量为 32，在高并发 mux stream 场景下容易满，导致 `acceptConnWorker` 阻塞。

**修复**: `make(chan tunnel.Conn, 32)` → `make(chan tunnel.Conn, 256)`

---

## 二、Debug 诊断模式设计

### 2.1 设计目标

- 提供运行时性能诊断能力，帮助定位转发慢、连接积压等问题
- 零侵入：仅在配置 `debug: true` 时启用，不影响正常运行性能
- 数据持久化到本地 `profile_debug/` 目录，便于离线分析

### 2.2 配置入口

**文件**: `proxy/config.go`

```json
{
  "debug": true
}
```

或 YAML:
```yaml
debug: true
```

`Config` 结构体新增字段：
```go
Debug bool `json:"debug" yaml:"debug"`
```

### 2.3 Profiler 架构

**文件**: `proxy/profiler.go`（新建）

```
┌─────────────────────────────────────────────┐
│              Profiler                        │
│                                              │
│  ┌──────────────────┐  ┌──────────────────┐ │
│  │  Snapshot Loop    │  │  Goroutine Dump  │ │
│  │  (每10秒)         │  │  Loop (每60秒)    │ │
│  └────────┬─────────┘  └────────┬─────────┘ │
│           │                     │            │
│           ▼                     ▼            │
│  ┌─────────────────────────────────────────┐ │
│  │         profile_debug/                   │ │
│  │  ├── metrics_snapshot.jsonl  (详细指标)   │ │
│  │  ├── summary.jsonl           (摘要指标)   │ │
│  │  └── goroutine_dump.txt      (堆栈快照)   │ │
│  └─────────────────────────────────────────┘ │
└─────────────────────────────────────────────┘
```

### 2.4 输出文件说明

#### `metrics_snapshot.jsonl`
每 10 秒写入一行完整的 JSON 快照，包含：
- `timestamp` / `unix_timestamp`: 时间戳
- `goroutines`: 当前 goroutine 数量
- `metrics`: 完整的 `MetricsSnapshot`，包含：
  - 连接生命周期计数（open/close/reason 分布）
  - 吞吐量分位数（P50/P95 上传/下载 bytes/s）
  - TLS 握手延迟分位数
  - Origin dial 延迟和失败分类
  - TTFB 分位数（首字节延迟）
  - Trojan auth 延迟和失败分类
  - Channel 水位（各 tunnel accept channel 的 depth/cap）
  - GC 统计（heap 使用、GC 暂停、CPU 占比）
  - UDP packet flow 统计
  - DNS 解析延迟
  - Mux stream 统计
  - TCP socket 遥测（RTT/CWND/Loss）
  - 反压事件计数
- `connections`: 聚合连接摘要（活跃数、总数、总速率）
- `channels`: Channel 水位采样

#### `summary.jsonl`
每 10 秒写入一行精简摘要，适合快速巡查：
```json
{"time":"15:04:05","goroutines":120,"active_conns":45,"total_conns":1200,
 "up_speed":5242880,"down_speed":10485760,
 "ttfb_p50_ms":12.50,"ttfb_p95_ms":85.30,
 "heap_mb":64.2,"gc_pause_ms":0.850,
 "conn_open_cps":15,"conn_close_cps":12,
 "backpressure_events":0}
```

#### `goroutine_dump.txt`
每 60 秒追加一次完整的 goroutine 堆栈快照，用于：
- 检测 goroutine 泄漏
- 定位阻塞点（哪些 goroutine 卡在 channel send/recv）
- 分析锁竞争

### 2.5 集成点

**文件**: `proxy/proxy.go`

- `Proxy` 结构体新增 `profiler *Profiler` 字段
- `NewProxyFromConfigData()`: 当 `cfg.Debug == true` 时创建 Profiler 并附加到 Proxy
- `Run()`: 调用 `profiler.Start()` 启动后台采集
- `Close()`: 调用 `profiler.Stop()` 停止采集

### 2.6 使用方式

1. 在配置文件中设置 `"debug": true`
2. 启动 trojan-go
3. 运行期间 `profile_debug/` 目录自动生成诊断数据
4. 用 `tail -f profile_debug/summary.jsonl` 实时观察关键指标
5. 用 `cat profile_debug/metrics_snapshot.jsonl | python3 -m json.tool` 查看详细快照
6. 用 `grep "goroutine" profile_debug/goroutine_dump.txt | tail -20` 检查 goroutine 趋势

### 2.7 诊断场景示例

| 现象 | 查看指标 | 定位方向 |
|------|----------|----------|
| 转发变慢 | `ttfb_p95_ms` 升高 | origin 响应慢或 DNS 慢 |
| 连接积压 | `active_conns` 持续增长 | channel 水位满、上游不消费 |
| 内存增长 | `heap_mb` 趋势 | goroutine 泄漏、buffer 未回收 |
| GC 频繁 | `gc_pause_ms` > 5ms | 调整 GOGC / mem_limit_mb |
| 反压 | `backpressure_events` > 0 | channel 容量不足 |
| 连接断开多 | `close_by_reason.reset` 高 | 网络质量差或被目标 RST |

---

## 三、文件变更清单（P0 + Debug 模式）

| 文件 | 操作 | 说明 |
|------|------|------|
| `common/io.go` | 修改 | RewindReader atomic.Bool 快路径 |
| `tunnel/trojan/client.go` | 修改 | 移除 100ms timer，改为 lazy flush |
| `tunnel/mux/server.go` | 修改 | connChan 32→256 |
| `proxy/config.go` | 修改 | 新增 Debug bool 字段 |
| `proxy/profiler.go` | 新建 | Profiler 诊断系统 |
| `proxy/proxy.go` | 修改 | 集成 Profiler 生命周期 |
| `common/io_test.go` | 新建 | RewindReader 快路径测试 |
| `proxy/profiler_test.go` | 新建 | Profiler 输出测试 |

---

## 四、多节点集群出口优选架构

### 4.1 问题背景

线上 debug profiler 数据显示，部分目标站点（如 Telegram `149.154.175.53`）的 origin dial P50 = 249ms、P95 = 281ms，远高于其他目标（Twitter 系列 3-6ms）。原因是当前 SERVER 模式固定从本地节点直连源站，如果该节点到目标的网络路径不优，延迟无法改善。

用户在多个地区部署了 trojan 节点，不同节点到同一目标的延迟差异显著。如果能动态选择到目标延迟最低的节点作为出口，可大幅降低用户感知延迟。

### 4.2 设计目标

- **智能出口选择**：SERVER 收到用户连接后，根据实时探测数据选择到目标延迟最低的出口节点
- **透明中继**：中间节点 → 出口节点的转发对客户端完全透明
- **自动故障转移**：出口节点不可达时自动切换到次优节点
- **最小侵入**：不改变客户端协议和配置，仅服务端新增集群能力
- **可观测**：debug 模式下记录出口选择决策和各节点延迟

### 4.3 整体架构

```
                        ┌─────────────────────────────────┐
                        │       Cluster Controller        │
                        │  (内嵌于每个 SERVER 节点)         │
                        │                                 │
                        │  ┌───────────┐ ┌─────────────┐  │
                        │  │ Prober    │ │ RouteTable  │  │
                        │  │ (探测器)   │ │ (路由表)     │  │
                        │  └─────┬─────┘ └──────┬──────┘  │
                        │        │ 定期探测       │ 查询    │
                        └────────┼───────────────┼────────┘
                                 │               │
        ┌────────────────────────┼───────────────┼────────────────────┐
        │                        │               │                    │
   ┌────▼────┐             ┌─────▼─────┐   ┌────▼────┐         ┌─────▼────┐
   │ Node A  │             │  Node B   │   │ Node C  │         │ Node D   │
   │ 东京    │             │  新加坡    │   │ 洛杉矶  │         │ 法兰克福 │
   │         │             │           │   │         │         │          │
   │ SERVER  │             │ SERVER+   │   │ SERVER  │         │ SERVER   │
   │ +Peer   │◄────────────│ Relay     │───►+Peer   │         │ +Peer    │
   └─────────┘  trojan     └───────────┘   └─────────┘         └──────────┘
                 relay                          │
                                                ▼
                                        ┌───────────────┐
                                        │ 149.154.175.53│
                                        │ (Telegram)    │
                                        └───────────────┘
```

**工作流程**（以 Telegram 为例）：
1. 用户连接到 Node B（新加坡，入口节点）
2. Node B 的 Router 发现目标是 `149.154.175.53`
3. 查询 RouteTable：Node C（洛杉矶）到该 IP 的 RTT 最低（15ms vs Node B 的 249ms）
4. Node B 通过 trojan relay 将连接中继到 Node C
5. Node C 作为出口节点直连 Telegram 服务器
6. 数据路径：用户 → Node B → Node C → Telegram

### 4.4 核心组件设计

#### 4.4.1 Peer 配置

**文件**：`tunnel/transport/config.go` 新增字段

```go
type PeerConfig struct {
    Name     string `json:"name" yaml:"name"`         // 节点名称，如 "tokyo-1"
    Host     string `json:"host" yaml:"host"`         // peer 地址
    Port     int    `json:"port" yaml:"port"`         // peer 端口
    Password string `json:"password" yaml:"password"` // trojan 认证密码
    // Weight 用于同延迟下的偏好。0 = 自动（按 RTT 排序）
    Weight   int    `json:"weight" yaml:"weight"`
}

type ClusterConfig struct {
    Enabled       bool         `json:"enabled" yaml:"enabled"`
    Peers         []PeerConfig `json:"peers" yaml:"peers"`
    ProbeInterval int          `json:"probe_interval" yaml:"probe-interval"` // 秒，最小 60，默认 120
    ProbeTimeout  int          `json:"probe_timeout" yaml:"probe-timeout"`   // 毫秒，默认 3000
    // 当 peer 延迟比本地低于此阈值（ms）才中继，避免微小差异触发中继
    RelayThreshold int         `json:"relay_threshold" yaml:"relay-threshold"` // 毫秒，默认 50
    // 延迟触发阈值：origin dial RTT 超过此值的目标 IP 才会被纳入探测列表
    // 低于此值的目标不会触发 peer 探测，降低探测对服务的压力
    LatencyThreshold int       `json:"latency_threshold" yaml:"latency-threshold"` // 毫秒，默认 100
    // 目标匹配规则：仅对匹配的目标启用集群出口选择
    // 格式同 router 规则：cidr:, domain:, geoip:, geosite: 等
    Targets       []string     `json:"targets" yaml:"targets"`
}
```

**配置示例**（`server.json`）：

```json
{
    "run_type": "server",
    "remote_addr": "0.0.0.0",
    "remote_port": 443,
    "password": ["user-password"],
    "cluster": {
        "enabled": true,
        "probe_interval": 120,
        "probe_timeout": 3000,
        "relay_threshold": 50,
        "latency_threshold": 100,
        "targets": [
            "cidr:149.154.160.0/20",
            "cidr:91.108.0.0/16",
            "domain:telegram.org",
            "geoip:ru"
        ],
        "peers": [
            {
                "name": "tokyo-1",
                "host": "tokyo.example.com",
                "port": 443,
                "password": "peer-shared-secret",
                "websocket": {"enabled": true, "host": "tokyo.example.com", "path": "/ws"},
                "ssl": {"sni": "tokyo.example.com", "verify": true}
            },
            {
                "name": "la-1",
                "host": "la.example.com",
                "port": 443,
                "password": "peer-shared-secret",
                "websocket": {"enabled": true, "host": "la.example.com", "path": "/ws"},
                "ssl": {"sni": "la.example.com", "verify": true}
            },
            {
                "name": "frankfurt-1",
                "host": "frankfurt.example.com",
                "port": 443,
                "password": "peer-shared-secret",
                "websocket": {"enabled": true, "host": "frankfurt.example.com", "path": "/ws"},
                "ssl": {"sni": "frankfurt.example.com", "verify": true}
            }
        ]
    }
}
```

#### 4.4.2 Prober（延迟探测器）

**文件**：`cluster/prober.go`（新建）

```go
// Prober 定期对每个 peer 和自身到指定目标的 TCP 连接延迟进行探测。
// 探测方式：TCP SYN 到目标 IP:port，测量握手完成时间。
//
// 关键设计：延迟触发 + 异步定时
// - 不主动探测所有可能的目标，而是由实际流量中 origin dial 延迟超过
//   latency_threshold 的目标 IP 动态注册到探测列表
// - 探测循环是异步定时的，不阻塞任何数据路径
// - probe_interval 最小值 60 秒，避免对 peer 节点造成压力
type Prober struct {
    ctx              context.Context
    cancel           context.CancelFunc
    peers            []PeerConfig
    staticTargets    []ProbeTarget       // 从 cluster.targets 解析出的固定目标
    dynamicTargets   sync.Map            // map[string]*DynamicTarget — 流量触发注册
    results          sync.Map            // map[targetKey][]PeerLatency
    interval         time.Duration       // 探测间隔，最小 60s
    timeout          time.Duration
    latencyThreshold time.Duration       // origin dial RTT 超过此值才注册探测
    localName        string              // 本节点名称
}

// DynamicTarget 是由实际流量延迟触发注册的探测目标
type DynamicTarget struct {
    Host          string
    Port          int
    FirstSeenAt   time.Time       // 首次观测到高延迟的时间
    LastDialRTT   time.Duration   // 最近一次 origin dial RTT
    ProbeCount    int64           // 已完成的探测次数
}

// ProbeTarget 是一个需要探测的目标
type ProbeTarget struct {
    Host string
    Port int
}

// PeerLatency 是一个节点到目标的延迟记录
type PeerLatency struct {
    PeerName  string
    RTT       time.Duration       // TCP connect RTT
    UpdatedAt time.Time
    Available bool
}
```

**延迟触发注册机制**：

不对所有 `cluster.targets` 中的目标主动探测，而是通过实际数据路径中的 origin dial 延迟动态发现慢目标：

```go
// RegisterSlowTarget 由 freedom.Client.DialConn 在 dial 完成后调用。
// 仅当 dialRTT > latencyThreshold 且目标匹配 cluster.targets 规则时注册。
// 该方法是无锁的（sync.Map），不会影响数据路径性能。
func (p *Prober) RegisterSlowTarget(host string, port int, dialRTT time.Duration) {
    if dialRTT < p.latencyThreshold {
        return
    }
    key := net.JoinHostPort(host, strconv.Itoa(port))
    if _, loaded := p.dynamicTargets.LoadOrStore(key, &DynamicTarget{
        Host:        host,
        Port:        port,
        FirstSeenAt: time.Now(),
        LastDialRTT: dialRTT,
    }); loaded {
        // 已存在，更新最新延迟
        if v, ok := p.dynamicTargets.Load(key); ok {
            v.(*DynamicTarget).LastDialRTT = dialRTT
        }
    }
    log.DebugKV("cluster: slow target registered",
        "host", host, "port", port,
        "dial_rtt_ms", dialRTT.Milliseconds(),
        "threshold_ms", p.latencyThreshold.Milliseconds())
}
```

**集成点**（`freedom.Client.DialConn` 中）：

```go
// 在 freedom.Client.DialConn 的 dialSplit 成功返回后：
if clusterProber != nil && dialDur > 0 {
    clusterProber.RegisterSlowTarget(host, port, dialDur)
}
```

**异步定时探测循环**：

探测循环完全异步，不阻塞任何数据路径。`probe_interval` 配置最小值为 60 秒，启动时强制 clamp：

```go
func NewProber(cfg ClusterConfig) *Prober {
    interval := time.Duration(cfg.ProbeInterval) * time.Second
    // 最小探测间隔 60 秒，防止对 peer 造成过大压力
    if interval < 60*time.Second {
        interval = 60 * time.Second
        log.Warnf("cluster: probe_interval clamped to minimum 60s (requested %ds)", cfg.ProbeInterval)
    }
    // ...
}

func (p *Prober) probeLoop() {
    ticker := time.NewTicker(p.interval)
    defer ticker.Stop()
    for {
        select {
        case <-p.ctx.Done():
            return
        case <-ticker.C:
            p.probeAll()
        }
    }
}

// probeAll 合并静态目标和动态注册的慢目标，并发探测。
// 整个过程在后台 goroutine 中异步执行，不阻塞任何连接处理。
func (p *Prober) probeAll() {
    // 合并探测目标
    targets := make([]ProbeTarget, len(p.staticTargets))
    copy(targets, p.staticTargets)
    
    p.dynamicTargets.Range(func(key, value interface{}) bool {
        dt := value.(*DynamicTarget)
        targets = append(targets, ProbeTarget{Host: dt.Host, Port: dt.Port})
        return true
    })
    
    if len(targets) == 0 {
        return // 无需探测
    }
    
    log.Infof("cluster: probing %d targets across %d peers", len(targets), len(p.peers))
    
    for _, target := range targets {
        // 并发探测所有 peer + 自身
        var wg sync.WaitGroup
        
        // 探测自身到目标的延迟
        wg.Add(1)
        go func() {
            defer wg.Done()
            rtt := p.probeDirect(target)
            p.updateResult(target, p.localName, rtt)
        }()
        
        // 探测每个 peer 到目标的延迟
        for _, peer := range p.peers {
            wg.Add(1)
            go func(peer PeerConfig) {
                defer wg.Done()
                rtt := p.probeViaPeer(peer, target)
                p.updateResult(target, peer.Name, rtt)
            }(peer)
        }
        wg.Wait()
    }
}
```

**动态目标淘汰**：

持续 30 分钟未被新流量触发（无新的高延迟 dial）的动态目标会从探测列表中移除，避免探测列表无限增长：

```go
func (p *Prober) evictStaleTargets() {
    cutoff := time.Now().Add(-30 * time.Minute)
    p.dynamicTargets.Range(func(key, value interface{}) bool {
        dt := value.(*DynamicTarget)
        if dt.FirstSeenAt.Before(cutoff) {
            p.dynamicTargets.Delete(key)
            log.DebugKV("cluster: evicted stale probe target", "host", dt.Host)
        }
        return true
    })
}
```

**探测方案 A 详细设计（推荐）**：

利用已有的 trojan 客户端协议栈，向 peer 节点发起到目标的 TCP 连接探测：

```go
func (p *Prober) probeViaPeer(peer PeerConfig, target ProbeTarget) time.Duration {
    // 1. 通过 peer 的 trojan 通道建立到目标的连接
    //    这走的是完整路径：本节点 → peer → 目标
    //    测量的是用户实际体验的端到端延迟
    start := time.Now()
    conn, err := p.dialViaPeer(peer, target)
    if err != nil {
        return -1 // 标记不可用
    }
    rtt := time.Since(start)
    conn.Close()
    return rtt
}
```

这种方式的优点：
- 测量的是真实的端到端延迟（本节点→peer→目标）
- 无需扩展 trojan 协议
- 每个 peer 只是一个普通的 trojan server，无需特殊配置

#### 4.4.3 RouteTable（路由表）

**文件**：`cluster/route_table.go`（新建）

```go
// RouteTable 维护目标到最优出口节点的映射。
// 由 Prober 定期更新，被 ClusterRouter 查询。
type RouteTable struct {
    mu             sync.RWMutex
    // key: 目标 IP 或 CIDR 前缀，value: 排序后的节点延迟列表
    entries        map[string][]PeerLatency
    localName      string
    relayThreshold time.Duration
}

// BestExit 返回到给定目标延迟最低的出口节点。
// 如果本地延迟已经足够好（差值 < relayThreshold），返回 "" 表示本地直连。
func (rt *RouteTable) BestExit(targetIP string) (peerName string, relayGain time.Duration) {
    rt.mu.RLock()
    defer rt.mu.RUnlock()
    
    entries := rt.match(targetIP)
    if len(entries) == 0 {
        return "", 0
    }
    
    var localRTT time.Duration
    var bestPeer PeerLatency
    for _, e := range entries {
        if e.PeerName == rt.localName {
            localRTT = e.RTT
        }
    }
    
    // 找延迟最低的可用 peer
    for _, e := range entries {
        if !e.Available || e.PeerName == rt.localName {
            continue
        }
        if bestPeer.PeerName == "" || e.RTT < bestPeer.RTT {
            bestPeer = e
        }
    }
    
    // 仅当 peer 延迟比本地低超过 relayThreshold 才中继
    if bestPeer.PeerName != "" && localRTT-bestPeer.RTT > rt.relayThreshold {
        return bestPeer.PeerName, localRTT - bestPeer.RTT
    }
    return "", 0
}
```

#### 4.4.4 ClusterRouter（集群路由决策）

**文件**：`cluster/router.go`（新建）

集成到 SERVER 模式的 origin dial 路径中。在 `freedom.Client.DialConn()` 之前拦截，判断是否需要中继。

```go
// ClusterRouter 嵌入到 server 的出站路径。
// 当 RouteTable 指示某个 peer 到目标更近时，将连接中继到该 peer。
type ClusterRouter struct {
    enabled    bool
    routeTable *RouteTable
    prober     *Prober
    peers      map[string]*PeerDialer  // name → dialer（预建立的 trojan 客户端栈）
    matcher    *TargetMatcher          // 判断目标是否在 cluster.targets 范围内
    metrics    *ClusterMetrics
}

// DialConn 是核心决策函数。
// 返回到目标的连接——要么本地直连，要么通过最优 peer 中继。
func (cr *ClusterRouter) DialConn(addr *tunnel.Address) (tunnel.Conn, string, error) {
    targetIP := addr.IP.String()
    
    // 1. 检查目标是否在集群管理范围
    if !cr.matcher.Match(addr) {
        return nil, "local", nil // 不在范围内，走本地
    }
    
    // 2. 查询路由表
    bestPeer, gain := cr.routeTable.BestExit(targetIP)
    if bestPeer == "" {
        return nil, "local", nil // 本地已经最优
    }
    
    // 3. 通过 peer 中继
    dialer, ok := cr.peers[bestPeer]
    if !ok {
        return nil, "local", nil
    }
    
    log.InfoKV("cluster relay",
        "target", addr.String(),
        "peer", bestPeer,
        "gain_ms", gain.Milliseconds())
    cr.metrics.RecordRelay(bestPeer, gain)
    
    conn, err := dialer.DialConn(addr)
    if err != nil {
        // 中继失败，回退到本地直连
        log.WarnKV("cluster relay failed, fallback to local",
            "peer", bestPeer, "err", err)
        cr.metrics.RecordRelayFallback(bestPeer)
        return nil, "local", nil
    }
    return conn, bestPeer, nil
}
```

#### 4.4.5 PeerDialer（Peer 连接器）

**文件**：`cluster/peer_dialer.go`（新建）

每个 peer 对应一个预初始化的 trojan 客户端栈，复用现有的 tunnel 层：

```go
// PeerDialer 封装到一个 peer 节点的 trojan 客户端连接。
// 本质上就是一个 trojan CLIENT 模式的出站栈。
type PeerDialer struct {
    name       string
    transport  *transport.Client
    tlsClient  tunnel.Client
    trojanCli  tunnel.Client
    ctx        context.Context
    cancel     context.CancelFunc
}

func NewPeerDialer(ctx context.Context, peer PeerConfig) (*PeerDialer, error) {
    // 构建到 peer 的 transport → tls → trojan 客户端栈
    // 复用现有的 tunnel 实现，仅需注入 peer 的地址和密码
    // ...
}

func (pd *PeerDialer) DialConn(addr *tunnel.Address) (tunnel.Conn, error) {
    // 通过 peer 的 trojan 客户端栈发起到目标的连接
    return pd.trojanCli.DialConn(addr, nil)
}
```

### 4.5 集成到 Server 出站路径

**文件**：`proxy/server/server.go` 修改

当前 SERVER 模式的 origin dial 路径：

```
proxy.relayConnLoop → sink.DialConn(addr) → freedom.Client.DialConn → net.Dialer.Dial → 目标
```

集成 ClusterRouter 后：

```
proxy.relayConnLoop → ClusterRouter.DialConn(addr)
    ├── 不在集群范围 / 本地最优 → freedom.Client.DialConn → 直连目标
    └── peer 更优 → PeerDialer.DialConn(addr) → peer trojan 通道 → peer 直连目标
```

具体修改方式：在 `proxy.Proxy` 中新增 `clusterRouter *cluster.ClusterRouter` 字段。`relayConnLoop` 中的 `p.sink.DialConn(addr)` 调用前插入集群路由决策：

```go
// 在 relayConnLoop 的 goroutine 内：
var outbound tunnel.Conn
var exitNode string

if p.clusterRouter != nil {
    relayConn, node, err := p.clusterRouter.DialConn(inbound.Metadata().Address)
    if err == nil && relayConn != nil {
        outbound = relayConn
        exitNode = node
    }
}

if outbound == nil {
    // 本地直连（原始路径）
    outbound, err = p.sink.DialConn(inbound.Metadata().Address, nil)
    exitNode = "local"
}
```

### 4.6 TargetMatcher（目标匹配器）

**文件**：`cluster/matcher.go`（新建）

复用现有 `tunnel/router` 的规则解析逻辑（`geoip:`, `geosite:`, `cidr:`, `domain:` 等），判断目标是否在集群管理范围内：

```go
type TargetMatcher struct {
    cidrs   []*v2router.CIDR
    domains []*v2router.Domain
}

// Match 判断给定地址是否在集群出口选择范围内
func (tm *TargetMatcher) Match(addr *tunnel.Address) bool {
    // 复用 tunnel/router 的 matchDomain / matchIP 逻辑
}
```

### 4.7 可观测性

#### 4.7.1 Debug Profiler 集成

`ProfileSnapshot` 新增字段：

```go
type ProfileSnapshot struct {
    // ... 现有字段 ...
    Cluster *ClusterSnapshot `json:"cluster,omitempty"`
}

type ClusterSnapshot struct {
    // 各 peer 到各探测目标的最新 RTT
    PeerLatencies []PeerLatencyRecord `json:"peer_latencies"`
    // 自上次快照以来的中继决策统计
    RelayCount       int64   `json:"relay_count"`
    FallbackCount    int64   `json:"fallback_count"`
    AvgGainMs        float64 `json:"avg_gain_ms"`
    // 当前路由表摘要
    ActiveRoutes     int     `json:"active_routes"`
    // 动态探测目标数量
    DynamicTargets   int     `json:"dynamic_targets"`
}
```

#### 4.7.2 summary.jsonl 新增字段

```json
{"time":"16:40:23", ..., "cluster_relays":5, "cluster_gain_avg_ms":180.5, "cluster_probed_targets":3, "peer_tokyo_rtt_ms":15, "peer_la_rtt_ms":45}
```

#### 4.7.3 HTTP API — 集群路由监控端点

**文件**：`api/httpapi/httpapi.go` 修改

新增端点 `/api/cluster`，返回集群路由的实时状态：

```go
// GET /api/cluster
// 返回集群路由状态：各 peer 延迟、活跃中继连接、探测目标列表
mux.HandleFunc("/api/cluster", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
    data := cluster.GlobalRouter().Snapshot()
    json.NewEncoder(w).Encode(data)
}))
```

**返回结构**：

```go
type ClusterAPIResponse struct {
    Enabled          bool                    `json:"enabled"`
    LocalNode        string                  `json:"local_node"`
    ProbeInterval    int                     `json:"probe_interval_sec"`
    LatencyThreshold int                     `json:"latency_threshold_ms"`
    
    // Peers 表：各节点状态和到各目标的延迟
    Peers            []PeerStatus            `json:"peers"`
    
    // 优化路由表：当前活跃的路由优化决策
    OptimizedRoutes  []OptimizedRouteEntry   `json:"optimized_routes"`
    
    // 中继统计
    Stats            ClusterStats            `json:"stats"`
}

type PeerStatus struct {
    Name        string `json:"name"`
    Host        string `json:"host"`
    Port        int    `json:"port"`
    Available   bool   `json:"available"`
    LastProbeAt string `json:"last_probe_at"`   // RFC3339
    // 该 peer 到各探测目标的 RTT
    TargetRTTs  []TargetRTT `json:"target_rtts"`
}

type TargetRTT struct {
    Target    string  `json:"target"`      // "149.154.175.53:443"
    RTTMs     float64 `json:"rtt_ms"`      // EWMA 平滑后的 RTT
    RawRTTMs  float64 `json:"raw_rtt_ms"`  // 最近一次探测的原始 RTT
    Available bool    `json:"available"`
}

// OptimizedRouteEntry 是一条优化路由决策记录
type OptimizedRouteEntry struct {
    Target       string  `json:"target"`         // 目标 IP 或域名
    LocalRTTMs   float64 `json:"local_rtt_ms"`   // 本节点直连延迟
    BestPeer     string  `json:"best_peer"`       // 最优出口节点名称
    PeerRTTMs    float64 `json:"peer_rtt_ms"`     // 最优节点延迟
    GainMs       float64 `json:"gain_ms"`         // 延迟改善（ms）
    GainPercent  float64 `json:"gain_percent"`    // 改善百分比
    RelayCount   int64   `json:"relay_count"`     // 通过此路由的中继次数
    Source       string  `json:"source"`          // "static" 或 "dynamic"（流量触发）
    FirstSeenAt  string  `json:"first_seen_at"`   // 首次发现时间
}

type ClusterStats struct {
    TotalRelays      int64   `json:"total_relays"`
    TotalFallbacks   int64   `json:"total_fallbacks"`
    AvgGainMs        float64 `json:"avg_gain_ms"`
    ProbeTargets     int     `json:"probe_targets"`      // 当前探测目标数
    StaticTargets    int     `json:"static_targets"`     // 配置的静态目标数
    DynamicTargets   int     `json:"dynamic_targets"`    // 流量触发的动态目标数
    LastProbeAt      string  `json:"last_probe_at"`
    LastProbeDurMs   float64 `json:"last_probe_dur_ms"`  // 上次探测耗时
}
```

**API 响应示例**：

```json
{
    "enabled": true,
    "local_node": "singapore-1",
    "probe_interval_sec": 120,
    "latency_threshold_ms": 100,
    "peers": [
        {
            "name": "tokyo-1",
            "host": "tokyo.example.com",
            "port": 443,
            "available": true,
            "last_probe_at": "2026-06-04T16:40:00+08:00",
            "target_rtts": [
                {"target": "149.154.175.53:443", "rtt_ms": 85.3, "raw_rtt_ms": 82.1, "available": true},
                {"target": "91.108.56.134:443", "rtt_ms": 92.1, "raw_rtt_ms": 95.0, "available": true}
            ]
        },
        {
            "name": "la-1",
            "host": "la.example.com",
            "port": 443,
            "available": true,
            "last_probe_at": "2026-06-04T16:40:00+08:00",
            "target_rtts": [
                {"target": "149.154.175.53:443", "rtt_ms": 15.2, "raw_rtt_ms": 14.8, "available": true},
                {"target": "91.108.56.134:443", "rtt_ms": 18.5, "raw_rtt_ms": 19.1, "available": true}
            ]
        }
    ],
    "optimized_routes": [
        {
            "target": "149.154.175.53",
            "local_rtt_ms": 249.0,
            "best_peer": "la-1",
            "peer_rtt_ms": 15.2,
            "gain_ms": 233.8,
            "gain_percent": 93.9,
            "relay_count": 47,
            "source": "dynamic",
            "first_seen_at": "2026-06-04T16:38:23+08:00"
        },
        {
            "target": "91.108.56.134",
            "local_rtt_ms": 180.5,
            "best_peer": "la-1",
            "peer_rtt_ms": 18.5,
            "gain_ms": 162.0,
            "gain_percent": 89.8,
            "relay_count": 23,
            "source": "static",
            "first_seen_at": "2026-06-04T16:35:00+08:00"
        }
    ],
    "stats": {
        "total_relays": 70,
        "total_fallbacks": 2,
        "avg_gain_ms": 197.9,
        "probe_targets": 5,
        "static_targets": 2,
        "dynamic_targets": 3,
        "last_probe_at": "2026-06-04T16:40:00+08:00",
        "last_probe_dur_ms": 312.5
    }
}
```

#### 4.7.4 Dashboard 集群路由监控表

**文件**：`api/httpapi/httpapi.go` 中的 `dashboardHTML` 修改

在现有 Dashboard 页面中新增 **Cluster Routes** Tab/Table，展示优化路由连接。UI 通过轮询 `/api/cluster` 端点获取数据。

**表格 1：Optimized Routes（优化路由表）**

| Target | Local RTT | Best Peer | Peer RTT | Gain | Gain % | Relays | Source |
|--------|-----------|-----------|----------|------|--------|--------|--------|
| 149.154.175.53 | 249.0 ms | la-1 | 15.2 ms | 233.8 ms | 93.9% | 47 | dynamic |
| 91.108.56.134 | 180.5 ms | la-1 | 18.5 ms | 162.0 ms | 89.8% | 23 | static |

- **Target**：目标 IP/域名
- **Local RTT**：本节点直连延迟（红色高亮 >200ms，黄色 100-200ms，绿色 <100ms）
- **Best Peer**：当前选中的最优出口节点
- **Peer RTT**：该 peer 到目标的延迟
- **Gain**：延迟改善量和百分比
- **Relays**：已通过此路由中继的连接数
- **Source**：`static`（配置固定）或 `dynamic`（流量触发发现）

**表格 2：Peer Status（节点状态表）**

| Peer | Host | Status | Targets | Avg RTT | Last Probe |
|------|------|--------|---------|---------|------------|
| tokyo-1 | tokyo.example.com:443 | ✓ Online | 5 | 88.7 ms | 2m ago |
| la-1 | la.example.com:443 | ✓ Online | 5 | 16.9 ms | 2m ago |
| frankfurt-1 | frankfurt.example.com:443 | ✗ Offline | 0 | — | 5m ago |

- **Status**：绿色圆点=在线，红色=离线（最近 3 次探测全部超时）
- **Targets**：该 peer 被探测的目标数量
- **Avg RTT**：到所有探测目标的平均延迟

**表格 3：Cluster Stats（集群统计摘要）**

```
┌─────────────────────────────────────────────────────┐
│  Total Relays: 70    Fallbacks: 2    Avg Gain: 198ms│
│  Probe Targets: 5 (2 static + 3 dynamic)            │
│  Probe Interval: 120s   Latency Threshold: 100ms    │
│  Last Probe: 2m ago (took 312ms)                     │
└─────────────────────────────────────────────────────┘
```

#### 4.7.5 Prometheus 指标

**文件**：`api/httpapi/prometheus.go` 修改

新增集群相关的 Prometheus 指标：

```
# 中继决策计数
trojan_cluster_relay_total{peer="la-1"} 47
trojan_cluster_relay_fallback_total{peer="la-1"} 1

# peer 到目标延迟
trojan_cluster_peer_rtt_ms{peer="la-1",target="149.154.175.53"} 15.2
trojan_cluster_peer_rtt_ms{peer="tokyo-1",target="149.154.175.53"} 85.3
trojan_cluster_local_rtt_ms{target="149.154.175.53"} 249.0

# 探测状态
trojan_cluster_probe_targets 5
trojan_cluster_probe_duration_ms 312.5
trojan_cluster_peer_available{peer="la-1"} 1
trojan_cluster_peer_available{peer="frankfurt-1"} 0

# 延迟增益
trojan_cluster_relay_gain_avg_ms 197.9
```

### 4.8 防环路设计

中继链路必须保证不成环（A→B→C→A）。采用**单跳限制**：

1. 通过 peer 中继的连接到达 peer 的 SERVER 后，peer 的 ClusterRouter 不再对该连接进行二次中继
2. 实现方式：trojan 协议头中复用现有的 Command 字段。中继连接使用一个标记（如 trojan metadata 中的扩展字段，或者专用的 peer 认证密码区分），peer 收到带标记的连接时直接走 freedom 直连，跳过 ClusterRouter

```go
// 在 ClusterRouter.DialConn 中：
// 检查连接是否来自 peer 中继（通过 context 传递标记）
if isRelayedConn(ctx) {
    return nil, "local", nil // 已经是中继连接，不再二次中继
}
```

### 4.9 实现分期

#### Phase 1：基础设施（MVP）
| 文件 | 操作 | 说明 |
|------|------|------|
| `cluster/config.go` | 新建 | ClusterConfig 结构体和配置解析 |
| `cluster/prober.go` | 新建 | TCP 直连探测 + 通过 peer 端到端探测 |
| `cluster/route_table.go` | 新建 | 路由表维护、EWMA 平滑、BestExit 查询 |
| `cluster/matcher.go` | 新建 | 目标匹配器，复用 v2router 规则 |
| `cluster/peer_dialer.go` | 新建 | 封装到 peer 的 trojan 客户端栈 |
| `cluster/router.go` | 新建 | ClusterRouter 决策引擎 |
| `proxy/proxy.go` | 修改 | 新增 clusterRouter 字段和集成逻辑 |
| `proxy/server/server.go` | 修改 | 初始化 ClusterRouter |
| `proxy/config.go` | 修改 | 注册 ClusterConfig |

#### Phase 2：可观测性
| 文件 | 操作 | 说明 |
|------|------|------|
| `cluster/metrics.go` | 新建 | 中继计数、延迟增益、回退统计、ClusterAPIResponse |
| `proxy/profiler.go` | 修改 | ProfileSnapshot 新增 ClusterSnapshot |
| `api/httpapi/httpapi.go` | 修改 | 新增 `/api/cluster` 端点，Dashboard 新增 Cluster Routes 表 |
| `api/httpapi/prometheus.go` | 修改 | 新增集群相关 Prometheus 指标 |

#### Phase 3：高级功能
| 文件 | 操作 | 说明 |
|------|------|------|
| `cluster/prober.go` | 修改 | 新增 ICMP ping 探测（需 root 权限） |
| `cluster/route_table.go` | 修改 | 按 CIDR 前缀聚合路由、基于历史数据的路由预测 |
| `cluster/peer_dialer.go` | 修改 | 连接池复用、mux 支持 |

### 4.10 配置部署示例

**场景**：3 个节点，用户从中国连接新加坡入口，访问 Telegram 时自动通过洛杉矶出口。

**新加坡节点**（入口，`server.json`）：
```json
{
    "run_type": "server",
    "local_addr": "0.0.0.0",
    "local_port": 443,
    "remote_addr": "127.0.0.1",
    "remote_port": 80,
    "password": ["user-password-hash"],
    "ssl": { "cert": "/path/to/cert.pem", "key": "/path/to/key.pem" },
    "cluster": {
        "enabled": true,
        "probe_interval": 30,
        "relay_threshold": 50,
        "targets": [
            "cidr:149.154.160.0/20",
            "cidr:91.108.0.0/16"
        ],
        "peers": [
            {
                "name": "tokyo-1",
                "host": "tokyo.example.com",
                "port": 443,
                "password": "peer-secret-tokyo",
                "websocket": {
                    "enabled": true,
                    "host": "tokyo.example.com",
                    "path": "/ws"
                },
                "ssl": {
                    "sni": "tokyo.example.com",
                    "verify": true
                }
            },
            {
                "name": "la-1",
                "host": "la.example.com",
                "port": 443,
                "password": "peer-secret-la",
                "websocket": {
                    "enabled": true,
                    "host": "la.example.com",
                    "path": "/ws"
                },
                "ssl": {
                    "sni": "la.example.com",
                    "verify": true
                }
            }
        ]
    },
    "debug": true
}
```

**洛杉矶节点**（出口，`server.json`）：
```json
{
    "run_type": "server",
    "local_addr": "0.0.0.0",
    "local_port": 443,
    "remote_addr": "127.0.0.1",
    "remote_port": 80,
    "password": ["user-password-hash", "peer-secret-la"],
    "ssl": { "cert": "/path/to/cert.pem", "key": "/path/to/key.pem" },
    "debug": true
}
```

注意：出口节点只需把 peer 密码加入 `password` 列表即可接受中继连接，无需配置 cluster。peer 中继连接就是普通的 trojan 连接，出口节点无需任何特殊处理。

### 4.11 延迟优化效果预估

基于线上 profiler 数据：

| 路径 | 当前延迟 | 优化后延迟 | 改善 |
|------|---------|-----------|------|
| 新加坡 → Telegram (149.154.175.53) | P50=249ms | P50≈65ms（经洛杉矶中继） | -74% |
| 新加坡 → Twitter | P50=3-6ms | P50=3-6ms（本地直连不变） | 无变化 |

预估基于：
- 新加坡 → 洛杉矶 RTT ≈ 150ms（trojan 通道）
- 洛杉矶 → Telegram ≈ 15ms（直连，同在美西）
- 端到端 ≈ 150 + 15 = 165ms，其中 trojan 协议开销 ≈ 1 个 RTT
- 总延迟 ≈ 65ms（中继 RTT 165ms 但只需单向传输，实际 TTFB 约为单程 + 回程首包）

注：实际效果取决于 peer 间网络质量，需上线后通过 profiler 验证。

---

## 五、sing-mux 兼容——支持 Shadowrocket 等第三方客户端的 MUX

### 5.1 问题背景

trojan-go 当前仅支持自有的 smux 多路复用协议。Shadowrocket（iOS）、sing-box 等第三方客户端使用 **sing-mux** 协议（sing-box 生态的多路复用标准），两者帧格式不同，导致 Shadowrocket 打开 mux 后无法连接 trojan-go 服务端。

> 参考源码：[SagerNet/sing-mux](https://github.com/SagerNet/sing-mux)
> 参考文档：[sing-box Multiplex 配置](https://sing-box.sagernet.org/configuration/shared/multiplex/)

### 5.2 协议对比

| 维度 | trojan-go mux | sing-mux (Shadowrocket/sing-box) |
|------|--------------|----------------------------------|
| Trojan Command | `0x01` (Connect) 或 `0x7f` | `0x7f` (Mux) |
| 目标地址 | `MUX_CONN:0` | `sp.mux.sing-box.arpa:444` |
| 协议头 | 无（直接 smux 帧） | Request Header：version + protocol + padding |
| 底层多路复用器 | smux only | smux / yamux / h2mux（可选） |
| 流级地址协议 | simplesocks | StreamRequest (flags + SOCKS addr) + StreamResponse |
| smux KeepAlive | 15s interval / 60s timeout | 禁用 |
| smux 库 | `github.com/xtaci/smux` | `github.com/sagernet/smux`（fork，帧格式兼容） |

### 5.3 sing-mux 协议详解

#### 5.3.1 连接建立流程

```
Client (Shadowrocket)                          Server (trojan-go)
  |                                                 |
  |-- Trojan Header -------------------------------->|
  |   Command = 0x7f (Mux)                          |
  |   Destination = sp.mux.sing-box.arpa:444        |  trojan server 识别为 Mux
  |                                                 |
  |-- sing-mux Request Header --------------------->|  读取 version + protocol
  |   Version (1B): 0 or 1                          |
  |   Protocol (1B): 0=smux, 1=yamux, 2=h2mux      |
  |   [V1] Padding (1B bool + 2B len + data)        |
  |                                                 |
  |== 底层多路复用 session =========================|  根据 protocol 创建 session
  |                                                 |
  |-- Stream Open --------------------------------->|
  |-- StreamRequest (flags + SOCKS addr) ---------->|  每个 stream 携带目标地址
  |<- StreamResponse (status=0 success) ------------|
  |== 双向数据传输 =================================|
```

#### 5.3.2 Request Header 格式

```
Version 0（无 padding）:
+----------+----------+
| Version  | Protocol |
| (1 byte) | (1 byte) |
|   0x00   | 0/1/2    |
+----------+----------+
共 2 字节

Version 1（支持 padding）:
+----------+----------+---------+-------------+-------------+
| Version  | Protocol | Padding | Padding Len | Padding     |
| (1 byte) | (1 byte) | (1B)    | (2B BE)     | (variable)  |
|   0x01   | 0/1/2    | bool    | uint16      | random data |
+----------+----------+---------+-------------+-------------+
共 5 + paddingLen 字节
```

Protocol 常量（`protocol.go`）：
```go
const (
    ProtocolSmux  = 0  // github.com/sagernet/smux
    ProtocolYAMux = 1  // github.com/hashicorp/yamux
    ProtocolH2Mux = 2  // 基于 golang.org/x/net/http2
)
```

#### 5.3.3 StreamRequest 格式（每个 stream 的第一次写入）

```
+----------+----------------------------+
| Flags    | Destination                |
| (2B BE)  | (SOCKS addr serialization) |
+----------+----------------------------+

Flags:
  bit 0: 1 = UDP, 0 = TCP
  bit 1: 1 = PacketAddr（每个 packet 附带独立地址）
```

SOCKS addr 序列化格式：
- `0x01` + 4 字节 IPv4 + 2 字节端口
- `0x03` + 1 字节域名长度 + 域名 + 2 字节端口
- `0x04` + 16 字节 IPv6 + 2 字节端口

#### 5.3.4 StreamResponse 格式（服务端第一次写入）

```
成功:  +--------+
       | 0x00   |
       +--------+

错误:  +--------+------------------+
       | 0x01   | Error Message    |
       |        | (varbin string)  |
       +--------+------------------+
```

#### 5.3.5 smux 配置差异

sing-mux 中 smux 的关键配置（`session.go:127`）：
```go
func smuxConfig() *smux.Config {
    config := smux.DefaultConfig()
    config.KeepAliveDisabled = true  // 禁用 KeepAlive
    return config
}
```

### 5.4 设计方案

#### 5.4.1 架构决策

**在 mux server 层做协议自动检测，按 trojan metadata 中的目标地址路由到不同处理器。**

理由：
- trojan server 已正确路由 `Command=0x7f → muxChan`
- 通过 `metadata.DomainName` 区分 100% 准确（`MUX_CONN` vs `sp.mux.sing-box.arpa`）
- 改动局限在 `tunnel/mux/` 和新建的 `tunnel/singmux/` 包，不影响其他模块

#### 5.4.2 协议检测

```go
// tunnel/mux/server.go — acceptConnWorker 修改
func (s *Server) acceptConnWorker() {
    for {
        conn, err := s.underlay.AcceptConn(&Tunnel{})
        if err != nil { ... }

        meta := conn.Metadata()
        if meta.Address.DomainName == "sp.mux.sing-box.arpa" {
            go s.handleSingMuxConn(conn)  // sing-mux 路径
        } else {
            go s.handleSmuxConn(conn)     // 现有 trojan-go smux 路径
        }
    }
}
```

#### 5.4.3 Server Stack 变化

sing-mux 路径**无需经过 simplesocks**，因为 StreamRequest 已包含目标地址。

当前 stack：
```
trojan → mux(smux) → simplesocks  (endpoint, trojan-go 客户端)
trojan                             (endpoint, 普通连接)
```

新增后：
```
trojan → mux(smux) → simplesocks  (endpoint, trojan-go 客户端)
trojan → singmux                   (endpoint, sing-mux 客户端如 Shadowrocket)
trojan                             (endpoint, 普通连接)
```

#### 5.4.4 trojan server 路由变更

为 singmux 独立路由，需在 trojan server 中分离 `Command=0x7f` 的处理：

```go
// tunnel/trojan/server.go
case Mux:  // Command = 0x7f
    if inboundConn.metadata.DomainName == "sp.mux.sing-box.arpa" {
        s.singMuxChan <- inboundConn   // sing-mux 客户端
    } else {
        s.muxChan <- inboundConn       // trojan-go 0x7f 风格
    }
```

trojan server AcceptConn 增加 singmux.Tunnel 类型：
```go
case *singmux.Tunnel:
    select {
    case t := <-s.singMuxChan:
        return t, nil
    case <-s.ctx.Done():
        return nil, common.NewError("trojan client closed")
    }
```

### 5.5 sing-mux Server 实现

#### 5.5.1 核心处理流程

```go
// tunnel/singmux/server.go

type Server struct {
    underlay tunnel.Server
    connChan chan tunnel.Conn
    ctx      context.Context
    cancel   context.CancelFunc
}

func (s *Server) acceptConnWorker() {
    for {
        conn, err := s.underlay.AcceptConn(&Tunnel{})
        if err != nil { ... }

        go func(conn tunnel.Conn) {
            defer conn.Close()

            // 1. 读取 sing-mux Request Header
            request, err := ReadRequest(conn)
            if err != nil {
                log.Error("singmux: read request: ", err)
                return
            }

            // 2. 根据 protocol 创建多路复用 session
            session, err := newServerSession(conn, request)
            if err != nil {
                log.Error("singmux: create session: ", err)
                return
            }
            defer session.Close()

            // 3. 接受 streams
            for {
                stream, err := session.Accept()
                if err != nil { return }

                go func(stream net.Conn) {
                    // 4. 读取 StreamRequest 获取目标地址
                    streamReq, err := ReadStreamRequest(stream)
                    if err != nil {
                        stream.Close()
                        return
                    }

                    // 5. 构造带正确 metadata 的 tunnel.Conn
                    addr := parseSocksAddr(streamReq.Destination)
                    singConn := &SingMuxConn{
                        rwc:    newResponseStream(stream), // 包装，首次 Write 自动发送 status=0
                        Conn:   conn,
                        target: addr,
                    }

                    select {
                    case s.connChan <- singConn:
                    case <-s.ctx.Done():
                        stream.Close()
                    }
                }(stream)
            }
        }(conn)
    }
}
```

#### 5.5.2 SingMuxConn

```go
// tunnel/singmux/conn.go

type SingMuxConn struct {
    rwc     io.ReadWriteCloser   // mux stream (wrapped with response logic)
    tunnel.Conn                  // underlying trojan conn
    target  *tunnel.Address
}

func (c *SingMuxConn) Metadata() *tunnel.Metadata {
    return &tunnel.Metadata{Address: c.target}
}
func (c *SingMuxConn) Read(p []byte) (int, error)  { return c.rwc.Read(p) }
func (c *SingMuxConn) Write(p []byte) (int, error) { return c.rwc.Write(p) }
func (c *SingMuxConn) Close() error                { return c.rwc.Close() }
```

#### 5.5.3 StreamResponse 自动发送

```go
// responseStream wraps a stream to automatically send status=0 on first Write
type responseStream struct {
    net.Conn
    responseSent bool
}

func (r *responseStream) Write(p []byte) (int, error) {
    if !r.responseSent {
        // Prepend status=0 (success) to first write
        buf := make([]byte, 1+len(p))
        buf[0] = 0x00 // statusSuccess
        copy(buf[1:], p)
        n, err := r.Conn.Write(buf)
        r.responseSent = true
        if n > 0 { n-- } // 扣除 status 字节
        return n, err
    }
    return r.Conn.Write(p)
}
```

#### 5.5.4 Session 创建

```go
// tunnel/singmux/session.go

func newServerSession(conn net.Conn, request *Request) (abstractSession, error) {
    switch request.Protocol {
    case ProtocolSmux:
        cfg := smux.DefaultConfig()
        cfg.KeepAliveDisabled = true  // 与 sing-mux 客户端保持一致
        return smux.Server(conn, cfg)
    case ProtocolYAMux:
        cfg := yamux.DefaultConfig()
        cfg.LogOutput = io.Discard
        return yamux.Server(conn, cfg)
    case ProtocolH2Mux:
        return newH2MuxServer(conn)
    default:
        return nil, fmt.Errorf("unknown protocol: %d", request.Protocol)
    }
}
```

### 5.6 新增文件清单

```
tunnel/singmux/
├── tunnel.go       # Tunnel 类型标识 + Name 常量
├── config.go       # 配置结构（padding 强制等）
├── protocol.go     # sing-mux Request/StreamRequest/StreamResponse 编解码
├── conn.go         # SingMuxConn + responseStream
├── session.go      # abstractSession + smux/yamux/h2mux 创建
├── h2mux.go        # h2mux server session（基于 http2.Server）
└── server.go       # singmux Server（接收 trojan conn → 输出 tunnel.Conn）
```

### 5.7 修改文件清单

| 文件 | 改动 |
|------|------|
| `tunnel/trojan/server.go` | 增加 `singMuxChan`，按 DomainName 分发 `Command=0x7f` |
| `proxy/server/server.go` | 在 stack 树中添加 `singmux` 分支作为 endpoint |
| `go.mod` | 添加 `github.com/hashicorp/yamux` 依赖 |

### 5.8 依赖分析

| 依赖 | 用途 | 状态 |
|------|------|------|
| `github.com/xtaci/smux` | smux 协议 | 已有 |
| `github.com/hashicorp/yamux` | yamux 协议 | **新增** |
| `golang.org/x/net/http2` | h2mux 协议（基于 HTTP/2） | 已有 |

**注意**：sing-mux 使用 `github.com/sagernet/smux`（fork），trojan-go 使用 `github.com/xtaci/smux`。两者帧格式完全兼容，但 sagernet 版本多了 `KeepAliveDisabled` 配置项。需要验证 `xtaci/smux` 是否支持此选项，否则需用极大 `KeepAliveInterval` 值替代或切换到 sagernet fork。

### 5.9 配置设计

服务端**无需显式配置**，sing-mux 支持由协议自动检测。可选配置：

```yaml
mux:
  enabled: true
  sing_mux:
    padding: false   # true = 强制要求 padding（拒绝非 padded 连接）
```

Shadowrocket 客户端配置：
```
协议: Trojan
地址: server.example.com:443
密码: user-password
传输: WebSocket
WS Host: server.example.com
WS Path: /ws
Mux: 开启
```

### 5.10 兼容性矩阵

| 客户端 | mux 协议 | 预期结果 |
|--------|---------|---------|
| trojan-go (mux=true) | smux via MUX_CONN | 正常（现有逻辑不变） |
| sing-box (protocol=smux) | sing-mux + smux | **新增支持** |
| sing-box (protocol=yamux) | sing-mux + yamux | **新增支持** |
| sing-box (protocol=h2mux) | sing-mux + h2mux | **新增支持** |
| Shadowrocket (mux=true) | sing-mux（待确认具体 protocol） | **新增支持** |
| Clash/v2rayN (mux) | mux.cool | 不支持（不同协议体系） |
| 所有客户端 (mux=false) | 无 | 正常（不受影响） |

### 5.11 实施步骤

1. **Phase 1: 协议层** — `tunnel/singmux/protocol.go`（编解码 + 单元测试）
2. **Phase 2: Session 层** — `tunnel/singmux/session.go` + `h2mux.go`（多协议 session）
3. **Phase 3: Server 层** — `tunnel/singmux/server.go`（stream 处理 + connChan 输出）
4. **Phase 4: 路由集成** — 修改 `tunnel/trojan/server.go` + `proxy/server/server.go`
5. **Phase 5: 测试** — 单元测试 + sing-box 客户端集成测试
6. **Phase 6: Shadowrocket 验证** — iOS 实机测试

### 5.12 风险评估

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| Shadowrocket 不使用 sing-mux | 仍然无法支持 Shadowrocket 的 mux | 先用 sing-box 客户端验证，再 Shadowrocket 测试。sing-box 客户端本身覆盖大量用户 |
| smux 库差异（xtaci vs sagernet） | KeepAliveDisabled 不支持 | 测试帧兼容性；必要时切换到 sagernet fork |
| h2mux 依赖 http2 内部 API | 版本升级风险 | 可先只支持 smux + yamux，h2mux 标记为实验性 |
| sing-mux 协议未来变更 | 新版本可能不兼容 | 按 Version 字段做版本检查，目前支持 V0 + V1 |
| 与集群路由的交互 | sing-mux 连接也需要支持集群路由优选 | `SingMuxConn.Metadata()` 返回正确目标地址，集群路由决策在 proxy 层进行，与 mux 类型无关 |
