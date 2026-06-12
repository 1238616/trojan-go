# 集群出口优选 — 测试计划

## 一、单元测试

### 1.1 cluster/config_test.go

| 用例 | 说明 |
|------|------|
| TestConfigDefaults | 验证 RegisterConfigCreator 返回的默认值：ProbeInterval=120, ProbeTimeout=3000, RelayThreshold=50, LatencyThreshold=100, NodeName="local" |
| TestConfigParsePeers | 从 JSON 反序列化完整的 peers 配置，验证 websocket/ssl 子字段正确解析 |

### 1.2 cluster/matcher_test.go

| 用例 | 说明 |
|------|------|
| TestMatcherCIDR | 创建 CIDR 规则 "cidr:149.154.160.0/20"，验证 149.154.175.53 匹配、8.8.8.8 不匹配 |
| TestMatcherDomain | 规则 "domain:telegram.org"，验证 "telegram.org" 和 "api.telegram.org" 匹配，"example.com" 不匹配 |
| TestMatcherIP | 规则 "ip:149.154.175.53"，验证精确匹配 |
| TestMatcherEmpty | 空规则列表，任何地址都不匹配 |
| TestMatcherMultiRules | 组合多种规则类型，验证 OR 语义 |

### 1.3 cluster/route_table_test.go

| 用例 | 说明 |
|------|------|
| TestRouteTableUpdate | Update 后 GetAllEntries 包含正确的 peer 记录 |
| TestRouteTableEWMA | 连续 Update 同一 peer，验证 RTT 被 EWMA 平滑（alpha=0.3） |
| TestRouteTableBestExitBasic | local=250ms, peerA=50ms, threshold=50ms → BestExit 返回 peerA |
| TestRouteTableBestExitBelowThreshold | local=80ms, peer=60ms, threshold=50ms → 差值 20ms < 50ms → 返回 "" |
| TestRouteTableBestExitUnavailable | peer RTT=-1（不可用）→ 返回 "" |
| TestRouteTableStaleEntry | 设置 UpdatedAt 为 15 分钟前 → 该记录被忽略 |

### 1.4 cluster/metrics_test.go

| 用例 | 说明 |
|------|------|
| TestMetricsRecordRelay | RecordRelay 3 次后 Snapshot: TotalRelays=3, AvgGainMs 正确 |
| TestMetricsRecordFallback | RecordRelayFallback 后计数正确 |
| TestMetricsRecordProbe | RecordProbe 后 LastProbeAt 和 LastProbeDurMs 有值 |
| TestMetricsConcurrency | 并发 100 goroutine 调用 RecordRelay，无 race |

### 1.5 cluster/peer_dialer_test.go

| 用例 | 说明 |
|------|------|
| TestHexSHA224 | 验证 SHA-224 哈希输出与已知值一致 |
| TestTrojanHeaderFormat | 构造 PeerDialer，调用内部 writeTrojanHeader 到 bytes.Buffer，验证格式：56字节hex + CRLF + CMD(0x01) + ATYP + ADDR + PORT + CRLF |
| TestPeerDialerCreateWithWebsocket | 验证 wsEnable=true 时 wsPath 和 wsHost 正确填充 |
| TestPeerDialerCreateWithoutWebsocket | 验证 wsEnable=false 时不设置 ws 字段 |

### 1.6 cluster/prober_test.go

| 用例 | 说明 |
|------|------|
| TestProberIntervalClamp | ProbeInterval=30 → 被 clamp 到 60s |
| TestProberRegisterSlowTarget | dialRTT > threshold → dynamicTargets 注册成功 |
| TestProberRegisterBelowThreshold | dialRTT < threshold → 不注册 |
| TestProberEvictStaleTargets | 设置 FirstSeenAt 为 31 分钟前 → 被淘汰 |
| TestProberDynamicTargetRefresh | 再次注册同一 target → FirstSeenAt 更新 |

### 1.7 cluster/router_test.go

| 用例 | 说明 |
|------|------|
| TestRouterDialConnNotInTargets | 目标不在 matcher 范围 → 返回 (nil, "local", nil) |
| TestRouterDialConnLocalOptimal | routeTable 中 local 最优 → 返回 "local" |
| TestRouterSnapshotStructure | 验证 Snapshot() 返回完整的 ClusterAPIResponse 结构 |

---

## 二、集成测试

### 2.1 TestClusterEndToEnd

**目标**：验证从配置加载到路由决策的完整流程。

**步骤**：
1. 构造带 cluster 配置的 JSON
2. 调用 `config.WithJSONConfig(ctx, data)` 解析
3. 从 context 获取 cluster.Config，验证字段正确
4. 创建 ClusterRouter，验证 peerDialers 数量
5. 手动 Update routeTable 模拟探测结果
6. 调用 DialConn 验证路由决策

### 2.2 TestClusterConfigRegistration

**目标**：验证 cluster config 的 init() 注册不与 proxy config 冲突。

**步骤**：
1. `config.FromContext(ctx, "CLUSTER")` → 返回 *cluster.Config
2. `config.FromContext(ctx, "PROXY")` → 返回 *proxy.Config
3. 两者独立，互不影响

---

## 三、WebSocket 连接测试

### 3.1 TestPeerDialerWebsocketHandshake

**目标**：验证 PeerDialer 能通过完整 TCP→TLS→WS→Trojan 栈连接到真实 peer。

**前置条件**：需要一个运行中的 trojan-go server（websocket 模式）。

**步骤**：
1. 启动本地 trojan-go server（配置 websocket.enabled=true, path="/ws"）
2. 创建 PeerDialer，指向 localhost:server_port
3. 调用 `DialConn` 连接到一个可达的目标（如 1.1.1.1:80）
4. 验证连接建立成功
5. 通过连接发送 HTTP GET，验证收到响应

### 3.2 TestPeerDialerTLSVerify

**目标**：验证 SSL.Verify=true 时证书验证生效。

**步骤**：
1. PeerDialer 配置 verify=true，指向自签名证书的 server
2. 预期 DialConn 返回 TLS 握手错误

### 3.3 TestPeerDialerWebsocketPathMismatch

**目标**：验证 websocket path 不匹配时连接失败。

**步骤**：
1. Server 配置 path="/ws"
2. PeerDialer 配置 path="/wrong"
3. 预期 DialConn 返回 websocket 握手错误

---

## 四、探测与路由测试（需网络环境）

### 4.1 TestProbeDirectTCP

**目标**：验证 probeDirect 能正确测量到可达目标的 TCP RTT。

**步骤**：
1. 对 localhost 上启动的 TCP listener 做直连探测
2. 验证返回的 RTT > 0 且 < 100ms（本地连接）

### 4.2 TestProbeDirectTimeout

**目标**：验证不可达目标返回 -1。

**步骤**：
1. 对一个未监听的端口做探测，timeout=1s
2. 验证返回 -1

### 4.3 TestProbeViaPeerRelay

**前置条件**：2 个 trojan-go server 实例（websocket 模式），1 个 HTTP server。

**步骤**：
1. Server A（peer）运行在 localhost:4431
2. HTTP target 运行在 localhost:8080
3. PeerDialer 连接 Server A
4. 调用 Probe(target={localhost, 8080})
5. 验证 RTT > 0

---

## 五、多节点端到端测试（手动/CI）

### 5.1 场景：3 节点中继

**拓扑**：
```
Client → Node A (entry, port 4431) → Node B (exit, port 4432) → Target HTTP (port 8080)
```

**步骤**：
1. 启动 target HTTP server（返回 "OK"）
2. 启动 Node B：server 模式，password=["user-pw", "peer-secret"]，websocket enabled
3. 启动 Node A：server 模式，password=["user-pw"]，cluster enabled，peer=Node B
4. 手动 Update Node A 的 route table：target=localhost:8080, peerB RTT=5ms, local RTT=500ms
5. Client 通过 Node A 连接到 target
6. **验证**：流量经 Node B 中继（从 Node B 日志确认）
7. **验证**：Client 收到 "OK" 响应

### 5.2 场景：中继失败回退

**步骤**：
1. 同上拓扑，但 Node B 未启动
2. Route table 仍指向 Node B
3. Client 连接 target
4. **验证**：ClusterRouter 回退到 local 直连
5. **验证**：metrics.TotalFallbacks 增加

### 5.3 场景：动态目标注册

**步骤**：
1. 启动 Node A，cluster enabled，latency_threshold=50ms
2. target 在远端（模拟高延迟：使用 tc netem 或 sleep proxy）
3. Client 通过 Node A 连接 target
4. **验证**：Prober.dynamicTargets 中出现该 target
5. **验证**：后续探测循环包含该 target

---

## 六、性能与压力测试

### 6.1 TestRouteTableConcurrentReadWrite

100 个 writer goroutine 持续 Update + 100 个 reader goroutine 持续 BestExit，运行 5s，验证无 race（`go test -race`）。

### 6.2 TestMetricsConcurrentAccess

类似 6.1，针对 ClusterMetrics 的 RecordRelay/RecordFallback/Snapshot。

### 6.3 TestProberNoBlockDataPath

在 probeAll 运行期间，验证 RegisterSlowTarget 调用不阻塞（完成 < 1μs）。

---

## 七、监控 API 测试

### 7.1 TestClusterAPIEndpoint

**步骤**：
1. 启动带 cluster 的 proxy
2. GET /api/cluster
3. 验证 JSON 响应包含 enabled=true, peers 数组非空, stats 字段存在

### 7.2 TestClusterAPIDisabled

**步骤**：
1. 启动不带 cluster 的 proxy
2. GET /api/cluster
3. 验证返回 enabled=false

---

## 八、运行方式

```bash
# 单元测试（不需要网络）
go test -v -race ./cluster/ -run "TestConfig|TestMatcher|TestRouteTable|TestMetrics|TestHexSHA224|TestTrojanHeader|TestProber"

# 集成测试（需要本地 trojan server）
go test -v -tags full ./cluster/ -run "TestCluster"

# 完整测试（包括网络探测）
go test -v -tags full -race ./cluster/ ./proxy/

# 压力测试
go test -v -race -count=1 -timeout=30s ./cluster/ -run "Concurrent"
```

---

## 九、CI 集成建议

1. 单元测试：加入默认 CI pipeline，`go test -race ./cluster/`
2. 集成测试：使用 docker-compose 启动多节点 trojan-go 实例
3. 性能回归：在 CI 中运行 benchmark，对比 relay buffer 吞吐量
4. 覆盖率目标：cluster 包 >= 80% line coverage

---

## 十、sing-mux 兼容性测试

### 10.1 编译验证

- [ ] `go build -tags full ./...` 编译通过
- [ ] `GOOS=linux GOARCH=amd64 go build -tags full -o trojan-go-linux-amd64 .` 交叉编译成功

### 10.2 协议编解码单元测试

#### Request Header 解析

| 用例 | 输入 | 预期 |
|------|------|------|
| V0 smux | `[0x00, 0x00]` | Version=0, Protocol=smux, Padding=false |
| V0 yamux | `[0x00, 0x01]` | Version=0, Protocol=yamux |
| V1 无 padding | `[0x01, 0x00, 0x00]` | Version=1, Protocol=smux, Padding=false |
| V1 有 padding | `[0x01, 0x00, 0x01, paddingLen(2B), padding...]` | 正确跳过 padding |
| 非法版本 | `[0x02, 0x00]` | 返回 error |

#### StreamRequest 解析

| 用例 | 输入 | 预期 |
|------|------|------|
| TCP + IPv4 | `flags=0x00 + [01, IP4, port]` | Network=tcp, Address=IP:port |
| TCP + Domain | `flags=0x00 + [03, len, domain, port]` | Network=tcp, Address=domain:port |
| TCP + IPv6 | `flags=0x00 + [04, IP6, port]` | Network=tcp, Address=[IP]:port |
| UDP | `flags=0x01 + addr` | Network=udp |

#### responseStream

| 用例 | 预期 |
|------|------|
| 首次 Write "hello" | 底层收到 `[0x00, 'h','e','l','l','o']` |
| 第二次 Write "world" | 底层收到 `['w','o','r','l','d']`（无 status） |
| 并发首次 Write | 只有一次 status byte 被写入 |

### 10.3 trojan server 路由测试

- [ ] `DomainName="sp.mux.sing-box.arpa"` → 路由到 singMuxChan
- [ ] `DomainName="MUX_CONN"` → 路由到 muxChan（不受影响）
- [ ] `DomainName="google.com"` → 路由到 connChan（不受影响）
- [ ] `Command=Mux` → 路由到 muxChan（不受影响）

### 10.4 singmux session 创建测试

- [ ] smux 协议：客户端发 `Request{V0, smux}` → 正确创建 smux session
- [ ] yamux 协议：客户端发 `Request{V0, yamux}` → 正确创建 yamux session
- [ ] h2mux 协议：返回 "not yet supported" 错误

### 10.5 sing-box 客户端端到端测试

**准备**：部署新版 trojan-go，安装 sing-box CLI

sing-box 配置：
```json
{
  "outbounds": [{
    "type": "trojan",
    "server": "server.example.com",
    "server_port": 443,
    "password": "your-password",
    "tls": { "enabled": true, "server_name": "server.example.com" },
    "transport": { "type": "ws", "path": "/ws" },
    "multiplex": { "enabled": true, "protocol": "smux", "max_connections": 4 }
  }]
}
```

| 测试 | 方法 | 预期 |
|------|------|------|
| HTTP 访问 | `curl -x socks5://127.0.0.1:1080 https://www.google.com` | 200 OK |
| 长连接 | YouTube 视频播放 | 流畅不中断 |
| 多 stream 并发 | 同时 5 个 HTTP 请求 | 全部成功 |
| 协议切换 | 改 protocol=yamux | 正常工作 |
| 空闲恢复 | 空闲 5 分钟后请求 | session 可用 |

### 10.6 Shadowrocket 实机测试

配置：Trojan + WebSocket + Mux 开启

| 测试 | 预期 |
|------|------|
| Safari 访问 google.com | 正常加载 |
| Telegram 收发消息 | 即时到达 |
| YouTube 播放 | 流畅 |
| 后台 10 分钟后恢复 | 自动恢复 |
| WiFi/4G 切换 | 新 session 自动建立 |

**关键确认**：查看服务端日志 `singmux: new session protocol=` 确认 Shadowrocket 使用的 mux 协议类型。

### 10.7 兼容性回归

| 场景 | 预期 |
|------|------|
| trojan-go 客户端 mux=true | MUX_CONN 路径不受影响 |
| trojan-go 客户端 mux=false | 普通 Connect 不受影响 |
| UDP (Associate) | 不受影响 |
| 非 trojan 流量 | 正常 redirect |

### 10.8 稳定性测试

- [ ] 运行 24 小时无内存泄漏
- [ ] 单 session 100 并发 stream 无 panic
- [ ] singMuxChan 水位正常（dashboard 可观测）
- [ ] session 关闭后 goroutine 正确退出
- [ ] 与 cluster routing 共存正常

### 10.9 运行方式

```bash
# singmux 单元测试
go test -v -race ./tunnel/singmux/

# 编译验证
go build -tags full ./...
```
