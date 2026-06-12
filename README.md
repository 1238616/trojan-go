# Trojan-Go

使用 Go 实现的完整 Trojan 代理，兼容原版 Trojan 协议及配置文件格式。安全、高效、轻巧、易用。

Trojan-Go 支持[多路复用](#多路复用)提升并发性能；使用[路由模块](#路由模块)实现国内外分流；支持 [CDN 流量中转](#websocket)(基于 WebSocket over TLS)；支持使用 AEAD 对 Trojan 流量进行[二次加密](#aead-加密)(基于 Shadowsocks AEAD)；支持可插拔的[传输层插件](#传输层插件)，允许替换 TLS，使用其他加密隧道传输 Trojan 协议流量。

预编译二进制可执行文件可在 [Release 页面](https://github.com/1238616/trojan-go/releases) 下载。解压后即可直接运行，无其他组件依赖。

## 目录

- [功能概览](#功能概览)
- [架构设计](#架构设计)
- [项目结构](#项目结构)
- [使用方法](#使用方法)
- [配置说明](#配置说明)
- [特性详解](#特性详解)
  - [WebSocket CDN 中转](#websocket)
  - [多路复用 (smux / sing-mux)](#多路复用)
  - [路由模块](#路由模块)
  - [AEAD 二次加密](#aead-加密)
  - [传输层插件](#传输层插件)
  - [连接监控与流量仪表盘](#连接监控与流量仪表盘)
  - [Prometheus 指标导出](#prometheus-抓取)
  - [零拷贝 splice 加速](#零拷贝-splice-加速)
  - [集群出口优选](#集群出口优选)
  - [调试性能分析器](#调试性能分析器)
  - [运行时调优参数](#运行时调优参数)
- [sing-mux 兼容](#sing-mux-兼容shadowrocket--sing-box-mux-支持)
- [构建](#构建)
- [致谢](#致谢)

## 功能概览

### 兼容原版 Trojan

- TLS 隧道传输
- TCP / UDP 代理
- 透明代理（NAT 模式，基于 iptables）
- 对抗 GFW 被动检测 / 主动检测
- MySQL 数据持久化与用户权限认证
- 用户流量统计和配额限制

### 扩展特性

| 特性 | 说明 |
|------|------|
| 简易模式 | 通过命令行参数快速启动服务端/客户端，无需配置文件 |
| Socks5 / HTTP 自动适配 | 客户端本地监听自动识别 Socks5 和 HTTP 代理协议 |
| TProxy 透明代理 | 基于 TProxy 的 TCP / UDP 透明代理（Linux） |
| 多路复用 (smux) | 通过一条 TLS 隧道承载多条 TCP 连接，降低延迟 |
| sing-mux 兼容 | 支持 Shadowrocket / sing-box 客户端的 Mux 协议（smux / yamux） |
| 路由模块 | 基于 GeoIP / GeoSite 规则实现国内外分流 / 广告屏蔽 |
| WebSocket 传输 | 基于 WebSocket over TLS 实现 CDN 流量中转 |
| TLS 指纹伪造 | 基于 uTLS 对抗 GFW 针对 TLS Client Hello 的特征识别 |
| AEAD 二次加密 | 基于 Shadowsocks AEAD 对 Trojan 流量二次加密 |
| gRPC API | 用户管理、速度限制等管理接口 |
| 连接监控仪表盘 | 内嵌 Web UI，实时查看连接速率、流量历史曲线 |
| Prometheus 指标 | 60+ 指标导出，涵盖连接生命周期、吞吐量、TLS、splice 等 |
| 零拷贝 splice(2) | Linux 下自动对纯 TCP 连接使用内核 splice 转发 |
| 集群出口优选 | 多节点自动延迟探测，选择最优出口降低访问时延 |
| 可插拔传输层 | 支持 Shadowsocks SIP003 标准混淆插件 |
| YAML 配置 | 同时支持 JSON 和 YAML 配置文件格式 |
| 全平台支持 | 交叉编译支持 Linux / macOS / Windows / FreeBSD，20+ 平台目标 |

## 架构设计

Trojan-Go 采用**可组合隧道栈 (Composable Tunnel Stack)** 架构。每一层协议实现统一的 `Tunnel` 接口，可以按任意顺序组合叠加。

### 核心接口

```
tunnel.Conn        — TCP 连接抽象
tunnel.PacketConn  — UDP 数据包流抽象
tunnel.Client      — 隧道客户端（Dial 方向）
tunnel.Server      — 隧道服务端（Accept 方向）
tunnel.Tunnel      — 协议层工厂，负责在底层隧道之上创建上层客户端/服务端
```

### 数据流路径

**客户端：**

```
用户应用
  ↓ Socks5 / HTTP
[Adapter] → [Transport(TCP)] → [TLS/uTLS] → [WebSocket](可选)
  → [Shadowsocks AEAD](可选) → [Trojan 协议] → [Mux](可选)
  → [SimpleSOCKS] → [Router] → [Freedom] → 目标服务器
```

**服务端：**

```
客户端连接
  ↓
[Transport(TCP)] → [TLS] → [WebSocket](可选)
  → [Shadowsocks AEAD](可选) → [Trojan 协议认证]
  → [Mux / SingMux / MuxCool](可选) → [SimpleSOCKS]
  → [Freedom] → 目标服务器
                  ↓ (可选)
            [Cluster Router → Peer 节点中继]
```

### 代理引擎

`Proxy` 是核心中继引擎，负责在 `sources`（入站 Server 列表）和 `sink`（出站 Client）之间双向转发数据：

- `relayConnLoop()` — TCP 连接中继，支持 splice 零拷贝快速路径
- `relayPacketLoop()` — UDP 数据包中继，使用缓冲区池减少 GC 压力
- 可选挂载 `ClusterRouter`（集群路由）、`Profiler`（调试分析器）、`ConnMonitor`（连接监控）

## 项目结构

```
trojan-go/
├── main.go                  # 程序入口，解析命令行参数并分发到选项处理器
├── go.mod                   # Go 模块定义（Go 1.19+）
├── Makefile                 # 构建系统，支持 20+ 平台交叉编译
├── Dockerfile               # Docker 镜像构建
│
├── component/               # 构建标签组合（控制编译哪些模块）
│   ├── base.go              #   基础导入（日志、内存统计、版本）
│   ├── server.go            #   服务端代理 (tag: server/full/mini)
│   ├── client.go            #   客户端代理 (tag: client/full/mini)
│   ├── forward.go           #   端口转发
│   ├── nat.go               #   NAT 透明代理
│   ├── custom.go            #   自定义隧道栈
│   ├── api.go               #   gRPC API 服务
│   └── mysql.go             #   MySQL 认证后端
│
├── proxy/                   # 核心代理引擎
│   ├── proxy.go             #   中继逻辑（连接/数据包转发、splice、集群路由）
│   ├── stack.go             #   隧道栈构建器（组合协议层）
│   ├── config.go            #   代理配置（缓冲区、GC 调优等）
│   ├── buffer.go            #   缓冲区池管理
│   ├── profiler.go          #   调试性能分析器
│   ├── server/server.go     #   服务端隧道栈组装
│   ├── client/client.go     #   客户端隧道栈组装
│   ├── forward/             #   端口转发代理
│   ├── nat/                 #   NAT 透明代理（TProxy）
│   └── custom/              #   自定义代理（用户定义隧道栈）
│
├── tunnel/                  # 隧道协议实现（可组合的协议层）
│   ├── tunnel.go            #   核心接口定义与隧道注册
│   ├── adapter/             #   Socks5/HTTP 自动检测适配器
│   ├── transport/           #   原始 TCP 传输（底层）
│   ├── tls/                 #   TLS 加密层（含 uTLS 指纹伪造）
│   ├── websocket/           #   WebSocket 传输（CDN 中转）
│   ├── trojan/              #   Trojan 协议（认证、帧封装）
│   ├── mux/                 #   smux 多路复用
│   ├── singmux/             #   sing-mux 协议（Shadowrocket/sing-box 兼容）
│   ├── muxcool/             #   v2ray mux.cool 协议兼容
│   ├── shadowsocks/         #   Shadowsocks AEAD 加密层
│   ├── simplesocks/         #   Simple SOCKS 协议（mux 上层使用）
│   ├── socks/               #   SOCKS5 代理（客户端面向）
│   ├── http/                #   HTTP 代理（客户端面向）
│   ├── router/              #   路由模块（GeoIP/GeoSite 规则）
│   ├── freedom/             #   直连出口（目标连接层）
│   ├── dokodemo/            #   Dokodemo-door（透明代理接受层）
│   └── tproxy/              #   TProxy 透明代理（Linux）
│
├── cluster/                 # 多节点集群出口优选
│   ├── router.go            #   ClusterRouter 决策引擎
│   ├── prober.go            #   周期延迟探测（TCP→TLS→WS→Trojan）
│   ├── route_table.go       #   EWMA 平滑延迟路由表
│   ├── peer_dialer.go       #   Peer 节点连接拨号器
│   ├── matcher.go           #   目标匹配（CIDR / 域名 / IP 规则）
│   └── config.go            #   集群配置定义
│
├── api/                     # 外部 API
│   ├── control/             #   gRPC 控制服务
│   ├── httpapi/             #   HTTP REST API 与 Web 仪表盘
│   │   ├── httpapi.go       #     连接监控 HTTP 服务
│   │   ├── dashboard.go     #     内嵌 HTML5 仪表盘（Chart.js 可视化）
│   │   └── prometheus.go    #     Prometheus /metrics 端点
│   └── service/             #   gRPC 服务实现（用户管理等）
│
├── statistic/               # 用户认证与流量统计
│   ├── memory/              #   内存认证器
│   ├── mysql/               #   MySQL 认证后端
│   └── connmonitor/         #   连接监控（实时追踪、指标聚合）
│
├── config/                  # 配置解析框架（JSON/YAML）
├── common/                  # 共享工具（IO、splice、网络、GeoData）
├── log/                     # 日志框架（结构化日志）
├── option/                  # CLI 选项/标志处理器框架
├── easy/                    # 简易模式（命令行快速启动）
├── url/                     # URL 模式（trojan-go:// 链接解析）
├── redirector/              # 连接重定向器（插件支持）
├── version/                 # 版本信息
├── constant/                # 编译时常量（通过 -ldflags 注入）
├── example/                 # 示例配置与 systemd 服务文件
└── docs/                    # Hugo 文档站点
```

## 使用方法

### 1. 简易模式（命令行快速启动）

**服务端：**

```shell
sudo ./trojan-go -server \
    -remote 127.0.0.1:80 \
    -local 0.0.0.0:443 \
    -key ./your_key.key \
    -cert ./your_cert.crt \
    -password your_password
```

**客户端：**

```shell
./trojan-go -client \
    -remote example.com:443 \
    -local 127.0.0.1:1080 \
    -password your_password
```

### 2. 配置文件模式（推荐）

```shell
./trojan-go -config config.json
# 或
./trojan-go -config config.yaml
```

### 3. URL 模式

```shell
./trojan-go -url 'trojan-go://password@cloudflare.com/?type=ws&path=%2Fpath&host=your-site.com'
```

### 4. Docker 部署

```shell
docker run \
    --name trojan-go \
    -d \
    -v /etc/trojan-go/:/etc/trojan-go \
    --network host \
    p4gefau1t/trojan-go
```

或指定配置文件路径：

```shell
docker run \
    --name trojan-go \
    -d \
    -v /path/to/host/config:/path/in/container \
    --network host \
    p4gefau1t/trojan-go \
    /path/in/container/config.json
```

### 5. systemd 服务

安装后可使用 systemd 管理：

```shell
sudo systemctl enable trojan-go
sudo systemctl start trojan-go
```

## 配置说明

### 基础配置

**服务端** `server.json`：

```json
{
  "run_type": "server",
  "local_addr": "0.0.0.0",
  "local_port": 443,
  "remote_addr": "127.0.0.1",
  "remote_port": 80,
  "password": ["your_password"],
  "ssl": {
    "cert": "your_cert.crt",
    "key": "your_key.key",
    "sni": "www.your-domain.com"
  }
}
```

**客户端** `client.json`：

```json
{
  "run_type": "client",
  "local_addr": "127.0.0.1",
  "local_port": 1080,
  "remote_addr": "www.your-domain.com",
  "remote_port": 443,
  "password": ["your_password"]
}
```

**等价的 YAML 客户端** `client.yaml`：

```yaml
run-type: client
local-addr: 127.0.0.1
local-port: 1080
remote-addr: www.your-domain.com
remote-port: 443
password:
  - your_password
```

### 运行模式

| `run_type` | 说明 |
|-------------|------|
| `server` | 服务端，接受入站连接并转发到目标 |
| `client` | 客户端，接受本地应用连接并通过隧道转发到服务端 |
| `forward` | 端口转发，将指定端口的流量转发到远程 |
| `nat` | NAT 透明代理，配合 iptables 实现透明代理 |

## 特性详解

> 一般情况下，Trojan-Go 和原版 Trojan 互相兼容。但一旦使用以下扩展特性（如多路复用、WebSocket 等），则需要双方都使用 Trojan-Go。

### WebSocket

Trojan-Go 支持使用 TLS + WebSocket 承载 Trojan 协议，使得利用 CDN 进行流量中转成为可能。

服务端和客户端配置文件中同时添加 `websocket` 选项即可启用：

```json
"websocket": {
    "enabled": true,
    "path": "/your-websocket-path",
    "hostname": "www.your-domain.com"
}
```

可以省略 `hostname`，但服务端和客户端的 `path` 必须一致。服务端开启 WebSocket 后，可以同时支持 WebSocket 和一般 Trojan 流量。未配置 WebSocket 的客户端依然可以正常使用。

### 多路复用

在网络条件较差时，一次 TLS 握手可能花费较多时间。Trojan-Go 支持多路复用（基于 [smux](https://github.com/xtaci/smux)），通过一条 TLS 隧道连接承载多条 TCP 连接，减少 TCP 和 TLS 握手延迟，提升高并发场景下的性能。

> 启用多路复用不能提高链路速度，但能降低延迟、提升大量并发请求时的体验，例如浏览含有大量图片的网页。

在客户端配置中启用：

```json
"mux": {
    "enabled": true
}
```

只需开启客户端 mux 配置，服务端会自动检测并提供支持。

**支持的复用协议：**

| 协议 | 状态 |
|------|------|
| smux (trojan-go 原生) | ✅ 已支持 |
| yamux (sing-mux) | ✅ 已支持 |
| h2mux | ❌ 暂未支持 |

### 路由模块

Trojan-Go 客户端内建路由模块，可实现国内直连、海外代理等自定义路由。

**路由策略：**

| 策略 | 说明 |
|------|------|
| `Proxy` | 通过 TLS 隧道代理请求 |
| `Bypass` | 本地直连目标地址 |
| `Block` | 封锁，直接关闭连接 |

配置示例：

```json
"router": {
    "enabled": true,
    "bypass": [
        "geoip:cn",
        "geoip:private",
        "full:localhost"
    ],
    "block": [
        "cidr:192.168.1.1/24"
    ],
    "proxy": [
        "domain:google.com"
    ],
    "default_policy": "proxy"
}
```

### AEAD 加密

Trojan-Go 支持基于 Shadowsocks AEAD 对 Trojan 流量进行二次加密，确保 WebSocket 传输流量不被不可信 CDN 识别和审查：

```json
"shadowsocks": {
    "enabled": true,
    "password": "my-password"
}
```

服务端和客户端必须同时开启且密码一致。

### 传输层插件

Trojan-Go 支持可插拔传输层，并兼容 Shadowsocks [SIP003](https://shadowsocks.org/en/wiki/Plugin.html) 标准的混淆插件。以 `v2ray-plugin` 为例：

> **此配置并不安全，仅作为演示。**

服务端：

```json
"transport_plugin": {
    "enabled": true,
    "type": "shadowsocks",
    "command": "./v2ray-plugin",
    "arg": ["-server", "-host", "www.baidu.com"]
}
```

客户端：

```json
"transport_plugin": {
    "enabled": true,
    "type": "shadowsocks",
    "command": "./v2ray-plugin",
    "arg": ["-host", "www.baidu.com"]
}
```

### 连接监控与流量仪表盘

Trojan-Go 内置实时连接监控系统：

- **实时连接列表**：追踪每条 TCP 连接的目标地址、上下行速率、已传输字节数、持续时间与状态
- **聚合流量统计**：活跃连接数、总上行/下行速率、总流量
- **15 分钟历史曲线**：每秒采样一次，保留最近 900 个数据点，通过 Chart.js 可视化
- **内嵌 Web 仪表盘**：纯 HTML5 + JavaScript，无需额外部署前端
- **可选认证**：通过 `secret` 字段保护 API 与仪表盘

**配置示例：**

```json
"conn_monitor": {
    "enabled": true,
    "addr": "127.0.0.1",
    "port": 9090,
    "secret": "my-dashboard-secret"
}
```

**配置字段：**

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用连接监控 |
| `addr` | string | `127.0.0.1` | HTTP API 绑定地址 |
| `port` | int | `9090` | HTTP API 绑定端口 |
| `secret` | string | `""` | API 认证密钥，为空则无需认证 |

**API 端点：**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/connections` | 所有连接的实时信息（速率、字节、时长、状态） |
| GET | `/api/summary` | 聚合统计（活跃连接数、总速率、总流量） |
| GET | `/api/history` | 最近 15 分钟流量历史数据点（900 点） |
| GET | `/api/metrics` | 高级指标快照（JSON 格式） |
| POST | `/api/auth` | 验证 secret 并返回认证结果 |
| GET | `/metrics` | Prometheus 文本格式抓取端点 |
| GET | `/dashboard` | Web 仪表盘页面 |

**认证方式：**

配置了 `secret` 后，所有 `/api/*` 端点（除 `/api/auth`）需要凭证：

- HTTP Header: `Authorization: Bearer <secret>`
- URL 参数: `?token=<secret>`

仪表盘 `/dashboard` 页面自带登录界面。

**快速使用：**

```
http://127.0.0.1:9090/dashboard
```

或通过 curl：

```shell
# 无认证
curl http://127.0.0.1:9090/api/summary

# 有认证
curl -H "Authorization: Bearer my-dashboard-secret" http://127.0.0.1:9090/api/connections
```

**响应示例：**

`GET /api/summary`：

```json
{
  "total_connections": 12,
  "active_connections": 3,
  "total_upload_speed": 102400.5,
  "total_download_speed": 512000.8,
  "total_upload_bytes": 10485760,
  "total_download_bytes": 52428800
}
```

`GET /api/connections`：

```json
[
  {
    "id": "conn-1",
    "target": "google.com:443",
    "upload_bytes": 4096,
    "download_bytes": 65536,
    "upload_speed": 1024.0,
    "download_speed": 16384.0,
    "start_time": 1714276800,
    "duration": 30.5,
    "status": "active"
  }
]
```

### Prometheus 抓取

启用监控后，Prometheus 可直接抓取 `/metrics` 端点：

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'trojan-go'
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:9090']
    metrics_path: /metrics
```

**Prometheus 指标分类：**

| 类别 | 示例指标 | 说明 |
|------|----------|------|
| 连接生命周期 | `trojan_conn_open_total`, `trojan_conn_close_total`, `trojan_conn_close_by_reason_total` | 连接开关计数，按关闭原因分类 (eof/timeout/reset/auth_fail) |
| 吞吐量百分位 | `trojan_up_bps_p50`, `trojan_down_bps_p95` | 每连接上下行速率 P50/P95 (bytes/s) |
| TLS 握手 | `trojan_tls_handshake_total`, `trojan_tls_handshake_p50_ms` | 握手次数、失败数、会话恢复、延迟百分位 |
| 源站拨号 | `trojan_origin_dial_total`, `trojan_origin_dial_p95_ms` | 源站连接次数、失败分类、延迟 |
| TTFB | `trojan_ttfb_p50_ms`, `trojan_ttfb_p95_ms` | 首字节时间百分位 |
| Trojan 认证 | `trojan_auth_total`, `trojan_auth_failed_total` | 认证次数、失败分类、延迟百分位 |
| 通道水位 | `trojan_channel_depth`, `trojan_channel_cap` | 内部通道深度与容量 |
| 多路复用 | `trojan_mux_streams_active`, `trojan_mux_streams_per_conn_p95` | 活跃 mux 流数、每物理连接流数百分位 |
| 零拷贝 splice | `trojan_splice_bytes_total`, `trojan_splice_fallback_total` | splice 传输字节数、调用次数、回退次数 |
| TCP 遥测 | `trojan_tcp_rtt_p50_us`, `trojan_tcp_cwnd_p95` | TCP RTT (μs)、拥塞窗口、丢包事件 |
| 按用户 | `trojan_user_bytes_down_total{hash="abc"}` | 每用户连接数、认证失败、上下行字节 |
| 按目标 | `trojan_target_dials_total{host="google.com"}` | 每目标拨号次数、失败、延迟百分位 |
| 数据包流 | `trojan_packet_open_total`, `trojan_packet_pps_p50` | UDP 数据包流开关、PPS/BPS 百分位 |
| DNS | `trojan_dns_resolve_total`, `trojan_dns_resolve_p95_ms` | DNS 解析次数、失败数、延迟 |
| 反压 | `trojan_accept_drops_total`, `trojan_backpressure_events_total` | Accept 丢弃、反压事件计数 |
| Go 运行时 | `trojan_go_goroutines`, `trojan_go_heap_alloc_mb` | goroutine 数、堆内存、GC 暂停 |

> 所有 `_p50` / `_p95` 后缀指标基于蓄水池采样（4096 样本上限）进行百分位估算，适用于实时观测。

### 零拷贝 splice 加速

> 仅 Linux 平台支持。

在配置中启用 `enable_zero_copy`，代理引擎会自动对两端均为原始 TCP 连接的 relay 使用 Linux `splice(2)` 系统调用，通过内核管道转发数据，实现零用户态拷贝。非 TCP 连接（如 TLS、mux）自动回退到普通 `io.Copy`。

```json
{
  "run_type": "server",
  "enable_zero_copy": true,
  "...": "..."
}
```

启用后，`trojan_splice_bytes_total` 和 `trojan_splice_calls_total` 指标会记录 splice 传输的字节数和调用次数，`trojan_splice_fallback_total` 记录回退到用户态拷贝的次数。

### 集群出口优选

当在多个地区部署 Trojan-Go 服务端节点时，可启用集群出口优选。该功能自动探测各节点到目标站点的延迟，并将连接通过延迟最低的节点中继转发。

**典型场景**：用户连接新加坡入口节点，访问 Telegram 时自动经由洛杉矶节点出口（延迟从 249ms 降至 15ms）。

**入口节点配置：**

```json
{
    "run_type": "server",
    "local_addr": "0.0.0.0",
    "local_port": 443,
    "remote_addr": "127.0.0.1",
    "remote_port": 80,
    "password": ["user-password"],
    "ssl": {
        "cert": "your_cert.crt",
        "key": "your_key.key"
    },
    "cluster": {
        "enabled": true,
        "node_name": "singapore-1",
        "probe_interval": 120,
        "probe_timeout": 3000,
        "relay_threshold": 50,
        "latency_threshold": 100,
        "targets": [
            "cidr:149.154.160.0/20",
            "cidr:91.108.0.0/16"
        ],
        "peers": [
            {
                "name": "la-1",
                "host": "la.example.com",
                "port": 443,
                "password": "peer-shared-secret",
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
    }
}
```

**出口节点**（只需将 peer 密码加入 password 列表）：

```json
{
    "run_type": "server",
    "local_addr": "0.0.0.0",
    "local_port": 443,
    "remote_addr": "127.0.0.1",
    "remote_port": 80,
    "password": ["user-password", "peer-shared-secret"],
    "ssl": {
        "cert": "your_cert.crt",
        "key": "your_key.key"
    },
    "websocket": {
        "enabled": true,
        "path": "/ws",
        "host": "la.example.com"
    }
}
```

**配置字段：**

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用集群出口优选 |
| `node_name` | string | `"local"` | 本节点名称 |
| `probe_interval` | int | `120` | 探测间隔（秒），最小 60 |
| `probe_timeout` | int | `3000` | 探测超时（毫秒） |
| `relay_threshold` | int | `50` | 中继阈值（毫秒），peer 延迟比本地低超过此值才中继 |
| `latency_threshold` | int | `100` | 延迟触发阈值（毫秒），origin dial RTT 超过此值才纳入探测 |
| `targets` | []string | `[]` | 目标匹配规则，支持 `cidr:`, `domain:`, `ip:` |
| `peers` | []object | `[]` | peer 节点列表 |

**工作原理：**

1. Prober 定期探测各 peer 节点到目标的端到端延迟（TCP→TLS→WebSocket→Trojan 通道）
2. origin dial 延迟超过 `latency_threshold` 的目标会被动态注册到探测列表
3. RouteTable 使用 EWMA 平滑延迟数据，选择最优出口节点
4. 当 peer 延迟比本地低超过 `relay_threshold` 时，连接自动通过该 peer 中继
5. 中继失败时自动回退到本地直连

启用连接监控后，可通过 `GET /api/cluster` 查看集群路由状态。

### 调试性能分析器

在配置中设置 `"debug": true` 可启用调试性能分析器。启用后，代理会周期性地将性能快照写入 `profile_debug/` 目录，包括：

- goroutine 数量与状态
- 活跃连接数
- GC 暂停时间与堆内存
- 内部通道填充水位
- TTFB 分布
- 延迟采样

```json
{
  "run_type": "server",
  "debug": true,
  "...": "..."
}
```

> 仅用于诊断问题，不建议在生产环境长期开启。

### 运行时调优参数

以下参数可在配置文件顶层设置，用于微调代理行为：

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `relay_buffer_size` | int | `32768` (32 KiB) | TCP 中继缓冲区大小，范围 [4096, 1048576] |
| `backpressure_thresh` | float64 | `0` | 反压阈值 (0, 1]，通道填充率超过此值记录反压事件 |
| `enable_packet_pool` | bool | `true` | 启用 UDP 数据包缓冲区池，减少 GC 压力 |
| `enable_zero_copy` | bool | `false` | 启用 Linux splice(2) 零拷贝 TCP 中继 |
| `gogc` | int | `0` (Go 默认 100) | GC 目标百分比，100-400 适合长驻代理进程 |
| `mem_limit_mb` | int64 | `0` (禁用) | 软内存限制 (MB)，通过 `debug.SetMemoryLimit` 设置 |
| `debug` | bool | `false` | 启用调试性能分析器 |

配置示例：

```json
{
  "run_type": "server",
  "relay_buffer_size": 65536,
  "gogc": 200,
  "mem_limit_mb": 128,
  "enable_zero_copy": true,
  "...": "..."
}
```

## sing-mux 兼容（Shadowrocket / sing-box Mux 支持）

trojan-go 兼容 sing-mux 协议，支持 Shadowrocket 和 sing-box 客户端开启 Mux 多路复用。服务端通过 trojan 协议头中的目标地址自动识别客户端类型，**无需额外配置**。

### 工作原理

sing-mux 客户端连接服务端时，trojan 协议头中目标地址为 `sp.mux.sing-box.arpa:444`。trojan-go 检测到该魔术地址后，走 sing-mux 处理路径：

```
客户端 → nginx(TLS+WS) → trojan-go → 自动检测:
  目标="MUX_CONN"            → trojan-go 原生 smux 路径
  目标="sp.mux.sing-box.arpa" → sing-mux 路径（新增）
  其他                        → 普通连接
```

### nginx 配置要求

sing-mux 在一条 WebSocket 连接上复用多个 stream。nginx 的 `proxy_read_timeout` 和 `proxy_send_timeout` **必须**设置足够长（建议 1 小时），否则空闲时 nginx 会断开 WebSocket 导致 mux session 丢失：

```nginx
location /ws {
    proxy_pass         http://127.0.0.1:10240;
    proxy_http_version 1.1;
    proxy_set_header   Upgrade $http_upgrade;
    proxy_set_header   Connection "upgrade";
    proxy_set_header   Host $host;

    # 关键：防止 nginx 在空闲时断开 mux 长连接
    proxy_read_timeout    3600s;
    proxy_send_timeout    3600s;
    proxy_connect_timeout 60s;
}
```

### Shadowrocket 客户端配置

```
类型:     Trojan
地址:     server.example.com
端口:     443
密码:     your-password
传输:     WebSocket
WS Host:  server.example.com
WS Path:  /ws
Mux:      开启
```

### sing-box 客户端配置

```json
{
  "outbounds": [{
    "type": "trojan",
    "server": "server.example.com",
    "server_port": 443,
    "password": "your-password",
    "tls": {
      "enabled": true,
      "server_name": "server.example.com"
    },
    "transport": {
      "type": "ws",
      "path": "/ws",
      "headers": { "Host": "server.example.com" }
    },
    "multiplex": {
      "enabled": true,
      "protocol": "smux",
      "max_connections": 4,
      "min_streams": 4
    }
  }]
}
```

> **注意**：sing-box 的 `multiplex.protocol` 请使用 `"smux"` 或 `"yamux"`，`"h2mux"` 暂不支持。

### 故障排查

1. **Shadowrocket 无法连接**：检查 nginx `proxy_read_timeout` 是否已设置为 3600s
2. **服务端日志**：正常应出现 `singmux: new session protocol=smux ...`
3. **确认 mux 协议**：如日志显示 `protocol=h2mux`，说明客户端使用了暂不支持的协议
4. **回退测试**：关闭 Shadowrocket Mux 选项确认非 mux 模式正常
5. **现有客户端不受影响**：trojan-go 原生 mux 走独立路径，不受 sing-mux 改动影响

## 构建

> 请确保 Go 版本 >= 1.19

### 使用 Make 构建

```shell
git clone https://github.com/1238616/trojan-go.git
cd trojan-go
make
make install  # 安装 systemd 服务等（可选）
```

### 使用 Go 构建

```shell
go build -tags "full"
```

> **重要：** 必须指定 `-tags "full"`（或其他功能标签如 `client`、`server`、`mini`），否则编译出的二进制仅包含 `-version` 标志。

### Build Tags

| Tag | 说明 |
|-----|------|
| `full` | 包含所有功能模块 |
| `server` | 仅服务端 |
| `client` | 仅客户端 |
| `mini` | 最小化（客户端 + 服务端） |
| `custom` | 自定义隧道栈 |

### 交叉编译

Go 支持通过环境变量进行交叉编译，编译出的单个可执行文件不依赖其他组件：

**64 位 Linux：**

```shell
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "full" -trimpath -ldflags="-s -w -buildid="
```

**Apple Silicon (macOS arm64)：**

```shell
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -tags "full" -trimpath -ldflags="-s -w -buildid="
```

**64 位 Windows：**

```shell
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags "full" -trimpath -ldflags="-s -w -buildid="
```

**MIPS 路由器 (Linux mips softfloat)：**

```shell
CGO_ENABLED=0 GOOS=linux GOARCH=mips GOMIPS=softfloat go build -tags "client" -trimpath -ldflags="-s -w -buildid="
```

### 支持的平台

| 操作系统 | 架构 |
|----------|------|
| Linux | 386, amd64, arm (v5/v6/v7/v8), mips (soft/hard float), mipsle, mips64, mips64le |
| macOS | amd64, arm64 |
| Windows | 386, amd64, arm (v6/v7), arm64 |
| FreeBSD | 386, amd64 |

## 致谢

- [Trojan](https://github.com/trojan-gfw/trojan) — 原版 Trojan 代理
- [V2Fly](https://github.com/v2fly) — V2Ray 核心（路由、GeoIP/GeoSite 数据、mux 协议）
- [utls](https://github.com/refraction-networking/utls) — TLS 指纹伪造
- [smux](https://github.com/xtaci/smux) — 多路复用协议
- [go-tproxy](https://github.com/LiamHaworth/go-tproxy) — TProxy 透明代理

## License

[GNU General Public License v3.0](LICENSE)
