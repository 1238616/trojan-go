# Trojan-Go 代码设计与架构分析

> 本文档基于 `/Users/duqingyang/qoderwork/trojan-go-source` 源码进行结构、功能与架构分析。
> 仅作为代码理解与架构说明，不涉及对代码本身的改动。

---

## 1. 项目概述

**Trojan-Go** 是使用 Go 实现的完整 Trojan 代理实现，兼容原版 Trojan 协议及其配置文件格式。其核心目标是：在 GFW 主动/被动探测对抗下，提供安全、易部署、跨平台、模块化、可扩展的代理隧道。

- **语言/版本**：Go 1.17（`go.mod`）
- **模块路径**：`github.com/p4gefau1t/trojan-go`
- **构建入口**：`main.go`（使用 `flag` 解析命令行选项 → `option.PopOptionHandler` 派发）
- **构建系统**：`Makefile` + Go build tags（`client` / `server` / `mini` / `full` / `api` 等）
- **配置格式**：JSON 与 YAML 双支持（`gopkg.in/yaml.v3`）
- **部署形态**：单一可执行文件，零外部依赖；提供 Docker 镜像与 systemd 服务样例

主要兼容/扩展能力（来源 `README.md`）：

- TLS 隧道传输、UDP 代理、透明代理（NAT / TProxy）
- 多用户管理、流量统计与配额（内存 / MySQL）
- 多路复用 (smux)、WebSocket over TLS（CDN 中转）
- AEAD 二次加密（Shadowsocks AEAD）
- 可插拔传输层插件（Shadowsocks SIP003 兼容）
- 路由模块（GeoIP / GeoSite / CIDR / 域名规则）
- TLS 指纹伪造（`utls`）
- gRPC API 用户管理 + HTTP 实时连接监控/仪表盘

---

## 2. 目录结构与模块划分

```
trojan-go-source/
├── main.go                # 程序入口，触发 option handler 链
├── Makefile / Dockerfile  # 构建脚本
├── api/                   # API 层
│   ├── api.go             # API 模块统一入口
│   ├── control/           # gRPC 控制台 CLI
│   ├── httpapi/           # HTTP 仪表盘 + REST API（连接监控）
│   └── service/           # gRPC Server/Client (proto 定义在 api.proto)
├── common/                # 通用工具（错误、IO、网络、同步、geodata 加载）
│   └── geodata/           # geoip/geosite 二进制缓存与解码
├── component/             # build tag 注册中心（按 tag 注入对应子包）
│   ├── base.go            # 默认基础组件 (golog/memory statistic/version)
│   ├── client.go / server.go / nat.go / forward.go / custom.go / mysql.go / api.go / other.go
├── config/                # 配置反序列化框架（Creator + context 注入）
├── constant/              # 常量（版本、路径等）
├── docs/                  # Hugo 文档源码（中文）
├── easy/                  # "简易模式"启动入口（短参数自动生成配置）
├── example/               # 示例配置文件 + systemd unit
├── log/                   # 日志抽象 + 实现
│   ├── golog/             # 带颜色和缓冲的实现
│   └── simplelog/         # 极简实现
├── option/                # 命令行选项注册表（按 Priority 派发）
├── proxy/                 # 代理核心（请求/包中继 + 协议栈树形组合）
│   ├── proxy.go           # Proxy 主循环（TCP/UDP relay + 监控统计）
│   ├── stack.go           # 协议栈 Node 与构建器（树形 server / 链式 client）
│   ├── client/, server/, custom/, forward/, nat/  # 不同 run_type 注册
│   └── option.go, config.go
├── redirector/            # 防主动探测：将异常流量重定向到伪装站点
├── statistic/             # 流量统计 / 用户认证抽象
│   ├── statistics.go
│   ├── connmonitor/       # 实时连接监控（含历史曲线）
│   ├── memory/            # 内存型用户/统计实现
│   └── mysql/             # MySQL 持久化实现
├── test/                  # 集成测试（scenario + util）
├── tunnel/                # 各种隧道协议实现（核心可组合单元）
│   ├── tunnel.go          # Tunnel/Server/Client/Conn 核心接口
│   ├── metadata.go        # 地址、命令、ATYP、Trojan 元数据
│   ├── adapter/           # SOCKS/HTTP 接入适配器
│   ├── dokodemo/          # 任意端口固定转发
│   ├── freedom/           # 直连出口
│   ├── http/              # HTTP CONNECT 入站
│   ├── mux/               # smux 多路复用
│   ├── router/            # GeoIP / GeoSite / 自定义规则路由
│   ├── shadowsocks/       # AEAD 二次加密
│   ├── simplesocks/       # 简化的 SOCKS（用于复用层内部）
│   ├── socks/             # SOCKS5 入站
│   ├── tls/               # TLS（含 utls 指纹）
│   ├── tproxy/            # Linux 透明代理
│   ├── transport/         # 最底层 TCP / 插件入口
│   ├── trojan/            # Trojan 协议（核心）
│   ├── websocket/         # WebSocket 承载
├── url/                   # trojan-go:// URL 解析与生成
└── version/               # 版本信息打印
```

---

## 3. 核心架构：协议栈与隧道组合

Trojan-Go 的核心设计思想是 **「Tunnel 抽象 + 可组合协议栈」**。整个数据通路被建模为一棵（服务端）或一条链（客户端），每个节点是一个 `Tunnel`，封装了"基于下层 Server/Client 包装出新 Server/Client"的能力。

### 3.1 Tunnel 接口 (`tunnel/tunnel.go`)

```go
type Tunnel interface {
    Name() string
    NewClient(ctx, lower Client) (Client, error)
    NewServer(ctx, lower Server) (Server, error)
}

type Conn interface { net.Conn; Metadata() *Metadata }
type PacketConn interface { net.PacketConn; ReadWithMetadata(...); WriteWithMetadata(...) }

type Client interface { ConnDialer; PacketDialer; io.Closer }
type Server interface { ConnListener; PacketListener; io.Closer }
```

- 所有协议（TLS、Trojan、WebSocket、Shadowsocks、Mux、SimpleSocks、Router、Freedom、Transport、TProxy、SOCKS、HTTP、Adapter、Dokodemo）都实现了 `Tunnel` 接口，并通过 `RegisterTunnel(name, t)` 在 `init()` 中向全局注册。
- 上层 Tunnel 可基于下层 Tunnel 工作（例如 Trojan 基于 TLS / WebSocket，Mux 基于 Trojan，SimpleSocks 又基于 Mux）。
- `Tunnel` 之间是"透明"的——上层并不关心下层是 TLS 还是明文 + 插件。

### 3.2 协议栈构建 (`proxy/stack.go`)

服务端使用树形结构 (`Node`)，因为同一个 TLS 入口可能要分发到 Trojan 与 WebSocket 两个子栈：

```go
type Node struct {
    Name       string
    Next       map[string]*Node
    IsEndpoint bool
    Context    context.Context
    Server     tunnel.Server
    Client     tunnel.Client
}

// 用法：root.BuildNext("TLS").BuildNext("TROJAN").IsEndpoint = true
// FindAllEndpoints 收集所有"叶子"作为 source
```

客户端使用线性链：

```go
func CreateClientStack(ctx, []string{"TRANSPORT","TLS","TROJAN","MUX","SIMPLESOCKS"}) (tunnel.Client, error)
```

### 3.3 服务端协议栈（`proxy/server/server.go`）

```
                              ┌── (no plugin) TLS ──┬── (SS?) ──┬── TROJAN [endpoint]
                              │                     │          └── TROJAN ── MUX ── SIMPLESOCKS [endpoint]
TRANSPORT (含 plugin/raw TCP) ┤
                              └── WEBSOCKET ───────┬── (SS?) ──┬── TROJAN [endpoint]
                                                                └── TROJAN ── MUX ── SIMPLESOCKS [endpoint]
```

出口侧（sink）：默认 `freedom`，启用 router 时为 `freedom + router`，其中 `router` 决定 Proxy/Bypass/Block。

### 3.4 客户端协议栈（`proxy/client/client.go`）

入口（source）：`adapter` 自动识别 SOCKS5 / HTTP CONNECT 的两个 endpoint。

出口（sink）线性链：

```
TRANSPORT [→ TLS] [→ WEBSOCKET] [→ SHADOWSOCKS] → TROJAN [→ MUX → SIMPLESOCKS] [→ ROUTER]
```

### 3.5 数据中继核心（`proxy/proxy.go`）

`Proxy` 是单一中继单元，包含多个 source（服务端的不同叶子）与一个 sink（客户端栈）：

- `relayConnLoop`：对每个 source 启 goroutine，`AcceptConn` → `sink.DialConn(metadata.Address)` → 双向 `io.Copy`，并通过 `countingReader` 把字节数推送给 `connmonitor.Monitor`。
- `relayPacketLoop`：UDP 路径，使用 `ReadWithMetadata` / `WriteWithMetadata` 支持 UDP over Trojan。
- 启动时异步运行 `httpapi.RunHTTPAPI(ctx)`（实时仪表盘）。
- 每条连接生成 `conn-N` ID，注册到 `connmonitor.Global()`，连接结束自动 `Unregister`。

---

## 4. 关键组件详解

### 4.1 入口与命令选项 (`main.go` + `option/`)

- `option.Handler` 接口含 `Name / Handle / Priority`；各模块通过 `init()` 注册自己的 handler（如 `easy`、`url`、`config-file`、`api/control`）。
- `main` 循环 `PopOptionHandler` → `Handle()`，按优先级顺序尝试，成功一个即退出。即"命令选项即一种启动模式"。

### 4.2 配置框架 (`config/config.go`)

- 各模块通过 `RegisterConfigCreator(name, func()interface{}{...})` 注册"默认结构 + 默认值"。
- `WithJSONConfig` / `WithYAMLConfig` 把同一份用户配置反序列化到所有已注册的结构里，并以 `name + "_CONFIG"` 为 key 写入 `context.Context`。
- 模块运行时用 `config.FromContext(ctx, "MODULE")` 取出。这种"无中心 schema"的方式让各模块声明各自需要的字段，实现解耦。

### 4.3 Trojan 协议与元数据 (`tunnel/trojan/`, `tunnel/metadata.go`)

- `Metadata = Command + Address`。`Address` 与 SOCKS5 ATYP 兼容（IPv4 / IPv6 / DomainName）。
- `tunnel/trojan` 实现核心协议：服务端校验密码 hash（来自 `statistic.Authenticator`），失败则用 `redirector` 把流量"反代"到伪装站点（防主动探测）。
- 客户端发送 hex(SHA224(password)) + CRLF + Metadata + payload。

### 4.4 入站适配 (`tunnel/adapter`, `socks`, `http`, `dokodemo`, `tproxy`)

- `adapter` 接收 TCP，根据首字节嗅探 SOCKS5/HTTP，然后转交给 `socks` 或 `http` 子栈，作为客户端的两个统一 source endpoint。
- `dokodemo` 实现"任意端口端口转发"。
- `tproxy` 利用 Linux `IP_TRANSPARENT` / `IP_RECVORIGDSTADDR` 实现 TCP/UDP 透明代理（仅 Linux）。

### 4.5 多路复用与 SimpleSocks (`tunnel/mux`, `tunnel/simplesocks`)

- `mux` 基于 `xtaci/smux`：客户端按需建立物理连接，把多条逻辑流复用其上，降低 TLS 握手开销。
- `simplesocks` 是简化版 SOCKS（仅元数据帧），位于 `mux` 之上，用于在复用流上承载真实业务的目的地址。
- 服务端"双叶子"设计（`TROJAN` 与 `TROJAN→MUX→SIMPLESOCKS`）使得开/不开 mux 的客户端可同时连入。

### 4.6 WebSocket / TLS / utls (`tunnel/websocket`, `tunnel/tls`)

- `tls` 同时支持标准库 TLS（服务端）与 `refraction-networking/utls`（客户端 ClientHello 指纹伪造，对抗 GFW 的 TLS 特征识别）。
- `websocket` 在 TLS 之上提供 WS 隧道，兼容 CDN，必要时与原 Trojan 流量在同一端口共存（服务端按路径/Upgrade 头分流）。

### 4.7 Shadowsocks AEAD 二次加密 (`tunnel/shadowsocks`)

- 当 CDN 不可信时，对 Trojan 流量再做一层 AEAD 加密（基于 `shadowsocks/go-shadowsocks2`），防止可信任 CDN 之外的中间方探测。

### 4.8 路由模块 (`tunnel/router`)

- 客户端出口栈最末层。实现 `Proxy / Bypass / Block` 三策略。
- 基于 V2Fly geoip/geosite 二进制（由 `common/geodata` 加载并 LRU 缓存），并支持 `domain:`、`full:`、`cidr:`、`geoip:`、`geosite:` 等规则前缀。
- Bypass：本地 freedom 直连；Proxy：交由下层（trojan）走代理；Block：直接断连。

### 4.9 传输层与插件 (`tunnel/transport`)

- 最底层：原始 TCP listen / dial，或 `transport_plugin` 启用时通过子进程调用 SIP003 兼容插件（如 v2ray-plugin），把 TLS 替换为其他混淆传输。

### 4.10 重定向器 (`redirector/`)

- 服务端"主动探测对抗"核心：当请求未通过密码校验或协议解析失败时，把连接透传到 `remote_addr:remote_port` 指定的真实 Web 服务，使探测者认为这是普通 HTTPS 站点。

### 4.11 统计与认证 (`statistic/`)

- 抽象接口：`Authenticator`（验证密码 hash → User）+ `User`（添加/查询流量、限速、配额）。
- `memory/`：进程内实现，密码哈希查表。
- `mysql/`：通过定时同步把流量/配额持久化到 MySQL，支持多节点共用账户。
- `connmonitor/`：与认证解耦，按"逻辑连接"维度实时统计速率，并维护 15 分钟历史时间序列（900 点）。

### 4.12 HTTP 仪表盘 / REST API (`api/httpapi/`)

- 配置项 `conn_monitor` (`enabled / addr / port / secret`)。
- 端点：
  - `GET /api/connections`：实时连接列表（目标、上下行字节/速率、时长、状态）
  - `GET /api/summary`：聚合活跃连接数、总速率、总流量
  - `GET /api/history`：900 点 15 分钟历史
  - `POST /api/auth`：校验 secret
  - `GET /dashboard`：内嵌 Web 仪表盘（Chart.js）
- 鉴权：`Authorization: Bearer <secret>` 或 `?token=`，使用 `subtle.ConstantTimeCompare` 防时序侧信道。

### 4.13 gRPC 管理 API (`api/service/`, `api/control/`)

- `api.proto` 定义用户管理（增删查、流量与速率限制）、流量上报、列表导出等服务。
- `service/server.go` 在服务端进程内对接 `Authenticator`，`service/client.go` + `control/control.go` 提供 `trojan-go -api ...` 子命令以远程管理。

### 4.14 简易模式 (`easy/easy.go`) 与 URL 模式 (`url/`)

- `easy`：用 `-server -remote ... -local ... -password ... -cert ... -key ...` 等短参数自动构造内存配置。
- `url`：解析/生成 `trojan-go://password@host:port/?type=ws&path=...&host=...` 的分享链接。

### 4.15 日志与公共库 (`log/`, `common/`)

- `log/log.go` 定义 `Logger` 接口与全局 `Set*` 函数；默认实现 `golog`（带颜色、缓冲、可写文件）。
- `common/` 提供 `NewError`（带堆栈/链式 base）、IO 辅助、`Must`、地址/网络工具。

---

## 5. 启动与运行流程

```
$ trojan-go -config config.json
        │
        ▼
main.go: flag.Parse → option.PopOptionHandler() (按 Priority)
        │  匹配 config-file handler
        ▼
proxy/option.go: 读取文件 → proxy.NewProxyFromConfigData(data, isJSON)
        │
        ▼
config.WithJSONConfig/WithYAMLConfig: 用所有 RegisterConfigCreator 注册的结构去反序列化 → 注入 ctx
        │
        ▼
查 cfg.RunType (server/client/forward/nat/custom) → creators[upper(runType)]
        │
        ▼
proxy/server/server.go (示例) init():
   1) 构造 transport.NewServer (raw TCP / plugin)
   2) 构造 Node 树：transport → [tls] → [shadowsocks] → trojan / trojan→mux→simplesocks
                              └→ websocket → ...
   3) FindAllEndpoints → []tunnel.Server (sources)
   4) CreateClientStack → freedom [+ router] (sink)
   5) NewProxy(ctx, cancel, sources, sink)
        │
        ▼
Proxy.Run():
   - go httpapi.RunHTTPAPI(ctx)        // 仪表盘 HTTP server
   - relayConnLoop()                   // 每 source 一个 goroutine 循环 Accept 并双向 io.Copy
   - relayPacketLoop()                 // UDP 路径
   - <-ctx.Done()                      // 阻塞至取消
```

---

## 6. 设计亮点与可扩展性

1. **Tunnel = 可组合的中间件**：每个协议都是一层可插拔的中间件，新增协议 = 实现 `Tunnel` 接口 + `init()` 注册。客户端与服务端自动以"线性链"和"树"两种模式组合。
2. **配置的去中心化**：每个模块用 `RegisterConfigCreator` 自治声明所需字段，避免上帝对象式 Schema；新增字段不需要改总配置定义。
3. **基于 build tag 的可裁剪**：`component/*.go` 用 `//go:build` 控制是否注入对应模块，可生成 `client-only` / `mini` / `server-only` 的精简二进制。
4. **统一抽象 + 独立 sink/source**：`Proxy` 只关心 `tunnel.Server`/`tunnel.Client`，与具体协议无关；既支持反向代理 (`forward`)、客户端、服务端、NAT、自定义栈 (`custom`)。
5. **对抗 GFW 的多重手段**：
   - **被动**：TLS + 真实证书 + utls 客户端指纹伪装；
   - **主动**：失败请求由 `redirector` 反代到真实站点；
   - **流量特征**：可叠加 WebSocket、Shadowsocks AEAD、SIP003 插件混淆。
6. **可观测性**：
   - 日志（多级、可着色、可写文件）；
   - gRPC API（用户/流量管理）；
   - HTTP 仪表盘（实时连接 + 15 分钟历史曲线 + Chart.js Web UI + Bearer 认证）。
7. **多路复用**：smux + simplesocks，显著降低高并发下 TLS 握手延迟。
8. **多样的部署形态**：JSON/YAML 配置、easy 模式短参数、URL 一键导入、Docker、systemd（含 `trojan-go@.service` 多实例模板）。

---

## 7. 关键源码导航

| 主题 | 路径 |
|------|------|
| 程序入口 | `main.go:11` |
| 选项调度 | `option/option.go:17` |
| 协议栈核心 | `proxy/proxy.go:35`, `proxy/stack.go:19` |
| 服务端栈定义 | `proxy/server/server.go:23` |
| 客户端栈定义 | `proxy/client/client.go:24` |
| Tunnel 接口 | `tunnel/tunnel.go:71` |
| Trojan 元数据 | `tunnel/metadata.go:16` |
| Trojan 协议入口 | `tunnel/trojan/tunnel.go:1` |
| 配置框架 | `config/config.go:16` |
| 实时连接监控 | `statistic/connmonitor/monitor.go:59` |
| HTTP 仪表盘 | `api/httpapi/httpapi.go:42` |
| gRPC 服务 | `api/service/server.go`, `api/service/api.proto` |
| 路由器 | `tunnel/router/tunnel.go` 系列 |
| 重定向器 | `redirector/redirector.go` |
| URL 解析 | `url/share_link.go` |
| 简易模式 | `easy/easy.go` |
| build tag 注入 | `component/*.go` |

---

## 8. 总结

Trojan-Go 在原 Trojan 兼容性的基础上，采用 **「Tunnel 接口 + 树形协议栈 + 配置自注册 + build-tag 裁剪」** 的设计，把一个复杂的代理系统拆为高度正交的可组合单元。这种结构既保证了 Trojan 协议在主动/被动探测下的隐蔽性，又通过 Mux、WebSocket、AEAD、Router、Plugin、TProxy、gRPC API、HTTP 仪表盘等扩展，使其成为一个工程化、可观测、可远程管理、可跨平台部署的现代代理框架。
