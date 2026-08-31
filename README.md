# Trojan-Go [![Go Report Card](https://goreportcard.com/badge/github.com/p4gefau1t/trojan-go)](https://goreportcard.com/report/github.com/p4gefau1t/trojan-go) [![Downloads](https://img.shields.io/github/downloads/p4gefau1t/trojan-go/total?label=downloads&logo=github&style=flat-square)](https://img.shields.io/github/downloads/p4gefau1t/trojan-go/total?label=downloads&logo=github&style=flat-square)

使用 Go 实现的完整 Trojan 代理，兼容原版 Trojan 协议及配置文件格式。安全、高效、轻巧、易用。

Trojan-Go 支持[多路复用](#多路复用)提升并发性能；使用[路由模块](#路由模块)实现国内外分流；支持 [CDN 流量中转](#Websocket)(基于 WebSocket over TLS)；支持使用 AEAD 对 Trojan 流量进行[二次加密](#aead-加密)(基于 Shadowsocks AEAD)；支持可插拔的[传输层插件](#传输层插件)，允许替换 TLS，使用其他加密隧道传输 Trojan 协议流量。

预编译二进制可执行文件可在 [Release 页面](https://github.com/1238616/trojan-go/releases)下载。解压后即可直接运行，无其他组件依赖。

如遇到配置和使用问题、发现 bug，或是有更好的想法，欢迎加入 [Telegram 交流反馈群](https://t.me/trojan_go_chat)。

## 简介

**完整介绍和配置教程，参见 [Trojan-Go 文档](https://p4gefau1t.github.io/trojan-go)。**

Trojan-Go 兼容原版 Trojan 的绝大多数功能，包括但不限于：

- TLS 隧道传输
- UDP 代理
- 透明代理 (NAT 模式，iptables 设置参考[这里](https://github.com/shadowsocks/shadowsocks-libev/tree/v3.3.1#transparent-proxy))
- 对抗 GFW 被动检测 / 主动检测的机制
- MySQL 数据持久化方案
- MySQL 用户权限认证
- 用户流量统计和配额限制

同时，Trojan-Go 还扩展实现了更多高效易用的功能特性：

- 便于快速部署的「简易模式」
- Socks5 / HTTP 代理自动适配
- 基于 TProxy 的透明代理（TCP / UDP）
- 全平台支持，无特殊依赖
- 基于多路复用（smux）降低延迟，提升并发性能
- 自定义路由模块，可实现国内外分流 / 广告屏蔽等功能
- Websocket 传输支持，以实现 CDN 流量中转（基于 WebSocket over TLS）和对抗 GFW 中间人攻击
- TLS 指纹伪造，以对抗 GFW 针对 TLS Client Hello 的特征识别
- 基于 gRPC 的 API 支持，以实现用户管理和速度限制等
- **实时连接监控与流量仪表盘**，支持 REST API 与 Web UI，可查看每连接速率、15 分钟流量历史曲线
- **Prometheus 指标导出**，支持外部 Prometheus 抓取，涵盖连接生命周期、吞吐量百分位、TLS 握手、多路复用、按用户/按目标统计等 60+ 指标
- **多节点集群出口优选**，支持配置多个 peer 节点，自动探测延迟并选择最优出口，降低高延迟目标的访问时延；支持 peer 隧道多路复用（smux）、紧急回退负缓存与全链路拨号超时保护，路由决策稳定可复现
- 可插拔传输层，可将 TLS 替换为其他协议或明文传输，同时有完整的 Shadowsocks 混淆插件支持
- 支持对用户更友好的 YAML 配置文件格式

## 图形界面客户端

Trojan-Go 服务端兼容所有原 Trojan 客户端，如 Igniter、ShadowRocket 等。以下是支持 Trojan-Go 扩展特性（Websocket / Mux 等）的客户端：

- [Qv2ray](https://github.com/Qv2ray/Qv2ray)：跨平台客户端，支持 Windows / macOS / Linux，使用 Trojan-Go 核心，支持所有 Trojan-Go 扩展特性。
- [Igniter-Go](https://github.com/p4gefau1t/trojan-go-android)：Android 客户端，Fork 自 Igniter，将 Igniter 核心替换为 Trojan-Go 并做了一定修改，支持所有 Trojan-Go 扩展特性。

## 使用方法

1. 快速启动服务端和客户端（简易模式）

    - 服务端

        ```shell
        sudo ./trojan-go -server -remote 127.0.0.1:80 -local 0.0.0.0:443 -key ./your_key.key -cert ./your_cert.crt -password your_password
        ```

    - 客户端

        ```shell
        ./trojan-go -client -remote example.com:443 -local 127.0.0.1:1080 -password your_password
        ```

2. 使用配置文件启动客户端 / 服务端 / 透明代理 / 中继（一般模式）

    ```shell
    ./trojan-go -config config.json
    ```

3. 使用 URL 启动客户端（格式参见文档）

    ```shell
    ./trojan-go -url 'trojan-go://password@cloudflare.com/?type=ws&path=%2Fpath&host=your-site.com'
    ```

4. 使用 Docker 部署

    上游镜像 `p4gefau1t/trojan-go` **不包含本 fork 的任何改动**（cluster、singmux、muxcool、监控仪表盘等），请先从本仓库源码构建镜像：

    ```shell
    git clone https://github.com/1238616/trojan-go.git
    cd trojan-go
    docker build -t trojan-go .
    ```

    然后运行（配置文件挂载到 `/etc/trojan-go/config.json`）：

    ```shell
    docker run \
        --name trojan-go \
        -d \
        -v /etc/trojan-go/:/etc/trojan-go \
        --network host \
        trojan-go
    ```

   或者

    ```shell
    docker run \
        --name trojan-go \
        -d \
        -v /path/to/host/config:/path/in/container \
        --network host \
        trojan-go \
        /path/in/container/config.json
    ```

## 特性

一般情况下，Trojan-Go 和 Trojan 是互相兼容的，但一旦使用下面介绍的扩展特性（如多路复用、Websocket 等），则无法兼容。

### 移植性

编译得到的 Trojan-Go 单个可执行文件不依赖其他组件。同时，你可以很方便地编译（或交叉编译） Trojan-Go，然后在你的服务器、PC、树莓派，甚至路由器上部署；可以方便地使用 build tag 删减模块，以缩小可执行文件体积。

例如，交叉编译一个可在 mips 处理器、Linux 操作系统上运行的、只有客户端功能的 Trojan-Go，只需执行下面的命令，得到的可执行文件可以直接在目标平台运行：

```shell
CGO_ENABLED=0 GOOS=linux GOARCH=mips go build -tags "client" -trimpath -ldflags "-s -w -buildid="
```

完整的 tag 说明参见 [Trojan-Go 文档](https://p4gefau1t.github.io/trojan-go)。

### 易用

配置文件格式与原版 Trojan 兼容，但做了大幅简化，未指定的字段会被赋予默认值，由此可以更方便地部署服务端和客户端。以下是一个简单例子，完整的配置文件可以参见[这里](https://p4gefau1t.github.io/trojan-go)。

服务端配置文件 `server.json`：

```json
{
  "run_type": "server",
  "local_addr": "0.0.0.0",
  "local_port": 443,
  "remote_addr": "127.0.0.1",
  "remote_port": 80,
  "password": ["your_awesome_password"],
  "ssl": {
    "cert": "your_cert.crt",
    "key": "your_key.key",
    "sni": "www.your-awesome-domain-name.com"
  }
}
```

客户端配置文件 `client.json`：

```json
{
  "run_type": "client",
  "local_addr": "127.0.0.1",
  "local_port": 1080,
  "remote_addr": "www.your-awesome-domain-name.com",
  "remote_port": 443,
  "password": ["your_awesome_password"]
}
```

可以使用更简明易读的 YAML 语法进行配置。以下是一个客户端的例子，与上面的 `client.json` 等价：

客户端配置文件 `client.yaml`：

```yaml
run-type: client
local-addr: 127.0.0.1
local-port: 1080
remote-addr: www.your-awesome-domain_name.com
remote-port: 443
password:
  - your_awesome_password
```

### 日志

配置项 `log_level`（YAML: `log-level`）控制输出量，**默认 `1`（INFO）**：

| 级别 | 名称 | 预期输出量 |
|------|------|-----------|
| `0` | ALL/DEBUG | 排障用。每条连接的建立/路由/关闭明细、每个 mux 流、每次拨号与 DNS 解析等，高并发下输出量很大 |
| `1` | INFO（默认） | 只剩启动配置、证书/密钥事件、监听地址、集群决策中的异常兜底等低频事件；**逐连接的常规事件不再输出** |
| `2` | WARN | 认证失败、非法报文、拨号失败回退等异常 |
| `3` | ERROR | 仅错误 |
| `4` | FATAL | 仅致命错误 |
| `5` | OFF | 静默 |

默认级别下，一条正常连接**不再产生任何日志行**：TLS 接入、trojan 认证通过、路由分发、连接关闭流量汇总等逐连接事件全部降为 Debug；认证失败、未知命令、拨号失败等异常仍保留在 Warn/Error。

KV 结构化日志（`log.DebugKV`/`InfoKV` 等）在级别过滤**之前**先做级别判断，被过滤的消息不会执行字符串拼接与 `fmt.Sprint`，热路径上零分配。

### WebSocket

Trojan-Go 支持使用 TLS + Websocket 承载 Trojan 协议，使得利用 CDN 进行流量中转成为可能。

服务端和客户端配置文件中同时添加 `websocket` 选项即可启用 Websocket 支持，例如

```json
"websocket": {
    "enabled": true,
    "path": "/your-websocket-path",
    "hostname": "www.your-awesome-domain-name.com"
}
```

完整的选项说明参见 [Trojan-Go 文档](https://p4gefau1t.github.io/trojan-go)。

可以省略 `hostname`, 但服务端和客户端的 `path` 必须一致。服务端开启 Websocket 支持后，可以同时支持 Websocket 和一般 Trojan 流量。未配置 Websocket 选项的客户端依然可以正常使用。

由于 Trojan 并不支持 Websocket，因此，虽然开启了 Websocket 支持的 Trojan-Go 服务端可以兼容所有客户端，但如果要使用 Websocket 承载流量，请确保双方都使用 Trojan-Go。

### 多路复用

在很差的网络条件下，一次 TLS 握手可能会花费很多时间。Trojan-Go 支持多路复用（基于 [smux](https://github.com/xtaci/smux)），通过一条 TLS 隧道连接承载多条 TCP 连接的方式，减少 TCP 和 TLS 握手带来的延迟，以期提升高并发情景下的性能。

> 启用多路复用并不能提高测速得到的链路速度，但能降低延迟、提升大量并发请求时的网络体验，例如浏览含有大量图片的网页等。

你可以通过设置客户端的 `mux` 选项 `enabled` 字段启用它：

```json
"mux": {
    "enabled": true
}
```

只需开启客户端 mux 配置即可，服务端会自动检测是否启用多路复用并提供支持。完整的选项说明参见 [Trojan-Go 文档](https://p4gefau1t.github.io/trojan-go)。

### 路由模块

Trojan-Go 客户端内建一个简单实用的路由模块，以方便实现国内直连、海外代理等自定义路由功能。

路由策略有三种：

- `Proxy` 代理：将请求通过 TLS 隧道进行代理，由 Trojan 服务端与目的地址进行连接。
- `Bypass` 绕过：直接使用本地设备与目的地址进行连接。
- `Block` 封锁：不发送请求，直接关闭连接。

要激活路由模块，请在配置文件中添加 `router` 选项，并设置 `enabled` 字段为 `true`：

```json
"router": {
    "enabled": true,
    "bypass": [
        "geoip:cn",
        "geoip:private",
        "full:localhost"
    ],
    "block": [
        "cidr:192.168.1.1/24",
    ],
    "proxy": [
        "domain:google.com",
    ],
    "default_policy": "proxy"
}
```

完整的选项说明参见 [Trojan-Go 文档](https://p4gefau1t.github.io/trojan-go)。

### AEAD 加密

Trojan-Go 支持基于 Shadowsocks AEAD 对 Trojan 协议流量进行二次加密，以保证 Websocket 传输流量无法被不可信的 CDN 识别和审查：

```json
"shadowsocks": {
    "enabled": true,
    "password": "my-password"
}
```

如需开启，服务端和客户端必须同时开启并保证密码一致。

### 传输层插件

Trojan-Go 支持可插拔的传输层插件，并支持 Shadowsocks [SIP003](https://shadowsocks.org/en/wiki/Plugin.html) 标准的混淆插件。下面是使用 `v2ray-plugin` 的一个例子：

> **此配置并不安全，仅作为演示**

服务端配置：

```json
"transport_plugin": {
    "enabled": true,
    "type": "shadowsocks",
    "command": "./v2ray-plugin",
    "arg": ["-server", "-host", "www.baidu.com"]
}
```

客户端配置：

```json
"transport_plugin": {
    "enabled": true,
    "type": "shadowsocks",
    "command": "./v2ray-plugin",
    "arg": ["-host", "www.baidu.com"]
}
```

完整的选项说明参见 [Trojan-Go 文档](https://p4gefau1t.github.io/trojan-go)。

### 连接监控与流量仪表盘

Trojan-Go 内置了一套实时连接监控功能，包含：

- **实时连接列表**：追踪每条 TCP 连接的目标地址、上下行速率、已传输字节数、持续时间与状态
- **聚合流量统计**：活跃连接数、总上行/下行速率、总流量
- **15 分钟历史曲线**：每秒采样一次，保留最近 900 个数据点，通过 Chart.js 可视化
- **内嵌 Web 仪表盘**：纯 HTML5 + JavaScript，无需额外部署前端
- **可选认证**：通过 `secret` 字段保护 API 与仪表盘

在服务端配置中添加 `conn_monitor`（JSON）或 `conn-monitor`（YAML）即可启用。

**JSON 配置示例：**

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
    "key": "your_key.key"
  },
  "conn_monitor": {
    "enabled": true,
    "addr": "127.0.0.1",
    "port": 9090,
    "secret": "my-dashboard-secret"
  }
}
```

**YAML 配置示例：**

```yaml
run-type: server
local-addr: 0.0.0.0
local-port: 443
remote-addr: 127.0.0.1
remote-port: 80
password:
  - your_password
ssl:
  cert: your_cert.crt
  key: your_key.key
conn-monitor:
  enabled: true
  addr: 127.0.0.1
  port: 9090
  secret: my-dashboard-secret
```

**配置字段说明：**

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用连接监控 |
| `addr` | string | `127.0.0.1` | HTTP API 绑定地址 |
| `port` | int | `9090` | HTTP API 绑定端口 |
| `secret` | string | `""` (空) | API 认证密钥，为空则无需认证 |

**API 端点：**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/connections` | 获取所有 TCP 连接与 UDP 流的实时信息（类型、速率、字节、时长、状态） |
| GET | `/api/summary` | 获取聚合统计（活跃连接数、总速率、总流量） |
| GET | `/api/history` | 获取最近 15 分钟的流量历史数据点（900 点） |
| GET | `/api/metrics` | 获取高级指标快照（连接生命周期、吞吐量百分位、TLS 握手、通道水位、Go 运行时等） |
| POST | `/api/auth` | 验证 secret 并返回认证结果 |
| GET | `/metrics` | Prometheus 文本格式抓取端点（兼容 Prometheus / OpenMetrics 解析器） |
| GET | `/dashboard` | 内嵌 Web 仪表盘页面 |

**认证方式：**

配置了 `secret` 后，所有 `/api/*` 端点（除 `/api/auth`）需要在请求中提供凭证：

- HTTP Header: `Authorization: Bearer <secret>`
- 或 URL 参数: `?token=<secret>`

仪表盘 `/dashboard` 页面自带登录界面，输入 secret 后自动以 Bearer token 方式访问 API。

**快速使用：**

启用后，在浏览器中访问以下地址即可打开仪表盘：

```
http://127.0.0.1:9090/dashboard
```

或通过 curl 查询 API：

```shell
# 无认证
curl http://127.0.0.1:9090/api/summary

# 有认证
curl -H "Authorization: Bearer my-dashboard-secret" http://127.0.0.1:9090/api/connections
```

**响应示例：**

`GET /api/summary` 返回：

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

> **字段语义：** `total_connections` 为**累计值**——进程启动以来注册过的连接总数，单调递增，不随连接关闭而减少；当前在线连接数请看 `active_connections`。`total_upload_bytes` / `total_download_bytes` 同为进程启动以来的累计流量。

`GET /api/connections` 返回：

```json
[
  {
    "id": "conn-1",
    "target": "google.com:443",
    "type": "tcp",
    "upload_bytes": 4096,
    "download_bytes": 65536,
    "upload_speed": 1024.0,
    "download_speed": 16384.0,
    "start_time": 1714276800,
    "duration": 30.5,
    "status": "active"
  },
  {
    "id": "udp-3",
    "target": "8.8.8.8:53",
    "type": "udp",
    "upload_bytes": 512,
    "download_bytes": 2048,
    "upload_speed": 64.0,
    "download_speed": 256.0,
    "start_time": 1714276812,
    "duration": 18.2,
    "status": "active"
  }
]
```

> **UDP 流：** `type` 字段区分 `tcp` 连接与 `udp` 数据包流。UDP 流的字节数同样计入 `/api/summary` 的累计流量与 `/api/history` 的历史曲线；其 `target` 在首个数据包到达后从包元数据学习得到（注册时短暂显示为 `udp` 占位）。零长度数据报（DNS 保活、QUIC/游戏探测）会被正常转发，不会中断流。

#### Prometheus 抓取

启用监控后，Prometheus 可直接抓取 `/metrics` 端点：

```yaml
# prometheus.yml
scrape_configs:
  - job_name: 'trojan-go'
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:9090']
    metrics_path: /metrics
    # 如配置了 secret，添加 Bearer 认证
    # authorization:
    #   type: Bearer
    #   credentials: my-dashboard-secret
```

**Prometheus 指标分类：**

| 类别 | 示例指标 | 说明 |
|------|----------|------|
| 连接生命周期 | `trojan_conn_open_total`, `trojan_conn_close_total`, `trojan_conn_close_by_reason_total` | 连接开/关计数，按关闭原因分类（eof/timeout/reset/auth_fail） |
| 吞吐量百分位 | `trojan_up_bps_p50`, `trojan_down_bps_p95` | 每连接上下行速率 P50/P95（字节/秒） |
| TLS 握手 | `trojan_tls_handshake_total`, `trojan_tls_handshake_p50_ms` | TLS 握手次数、失败数、会话恢复、延迟百分位 |
| 源站拨号 | `trojan_origin_dial_total`, `trojan_origin_dial_p95_ms` | 源站连接次数、失败分类（dns/refused/timeout）、延迟 |
| TTFB | `trojan_ttfb_p50_ms`, `trojan_ttfb_p95_ms` | 首字节时间百分位 |
| Trojan 认证 | `trojan_auth_total`, `trojan_auth_failed_total` | 认证次数、失败分类、延迟百分位 |
| 通道水位 | `trojan_channel_depth`, `trojan_channel_cap` | 内部通道深度与容量（按 `name` 标签区分） |
| 多路复用 | `trojan_mux_streams_active`, `trojan_mux_streams_per_conn_p95` | 活跃 mux 流数、每物理连接流数百分位、队列深度 |
| 按用户 | `trojan_user_bytes_down_total{hash="abc"}` | 每用户连接数、认证失败、上下行字节（按 `hash` 标签区分） |
| 按目标 | `trojan_target_dials_total{host="google.com"}` | 每目标拨号次数、失败、上下行字节、延迟百分位 |
| 数据包流 | `trojan_packet_open_total`, `trojan_packet_pps_p50` | UDP 数据包流开/关；PPS/BPS 百分位按每秒每流采样（数据源为 connmonitor 中被中继的 UDP 流） |
| DNS | `trojan_dns_resolve_total`, `trojan_dns_resolve_p95_ms` | DNS 解析次数、失败数、延迟百分位 |
| 反压 | `trojan_backpressure_events_total` | 内部通道水位超阈值的反压事件计数 |
| Go 运行时 | `trojan_go_goroutines`, `trojan_go_heap_alloc_mb`, `trojan_go_gc_pause_last_ms` | goroutine 数、堆内存、GC 暂停 |

> **提示：** 所有带 `_p50` / `_p95` 后缀的指标基于蓄水池采样（4096 样本上限）进行百分位估算，适用于实时观测，非精确统计。

#### `GET /api/metrics` 响应示例

返回完整的高级指标快照（与 `/metrics` 同源，JSON 格式）：

```json
{
  "conn_open_total": 1024,
  "conn_close_total": 980,
  "open_cps": 5,
  "close_cps": 3,
  "up_bps_p50": 2048.0,
  "up_bps_p95": 65536.0,
  "down_bps_p50": 8192.0,
  "down_bps_p95": 131072.0,
  "tls_handshake_total": 500,
  "tls_handshake_failed": 2,
  "mux_streams_active": 12,
  "mux_streams_total": 340,
  "goroutines": 64,
  "heap_alloc_mb": 12.5
}
```

### 中继缓冲与内存占用

`proxy` 配置中的 `relay_buffer_size`（JSON；YAML 为 `relay-buffer-size`）调整每条 TCP 中继单个方向从缓冲池借用的缓冲区大小，默认 32 KiB，允许范围 [4 KiB, 1 MiB]（自动对齐到 1 KiB 的整数倍）。

需要注意：该缓冲区在**整条连接的生命周期内**被两个转发方向各自独占（读循环阻塞在 Read 上时缓冲区无法归还池中），缓冲池的规模因此等于「峰值并发 × 2」，内存占用随并发连接数线性增长，而不是随吞吐量复用：

> 中继缓冲内存 ≈ `relay_buffer_size` × 2（双向）× 峰值并发 TCP 连接数

| `relay_buffer_size` | 每连接占用 | 1,000 并发 | 10,000 并发 |
|---|---|---|---|
| 16 KiB | 32 KiB | ≈ 32 MiB | ≈ 320 MiB |
| 32 KiB（默认） | 64 KiB | ≈ 64 MiB | ≈ 640 MiB |
| 256 KiB | 512 KiB | ≈ 512 MiB | ≈ 5 GiB |
| 1 MiB（上限） | 2 MiB | ≈ 2 GiB | ≈ 20 GiB |

上表仅为中继缓冲本身，不含每条 UDP 流的 8 KiB 数据包缓冲、TLS/协议层缓冲与 Go 运行时开销。

推荐区间：

- 默认 32 KiB 适合绝大多数部署；
- 高延迟、高带宽积链路（卫星、跨洲线路）可调高到 64–256 KiB 以提升单连接吞吐，但请先按上面的公式结合峰值并发确认可用内存；
- 内存紧张且并发很高时可下调到 8–16 KiB，代价是系统调用次数增多、单连接吞吐下降。

### 集群出口优选

当你在多个地区部署了 Trojan-Go 服务端节点时，可以启用集群出口优选功能。该功能会自动探测各节点到目标站点的延迟，并将连接通过延迟最低的节点中继转发，从而降低用户感知延迟。

**典型场景**：用户连接新加坡入口节点，访问 Telegram 时自动经由洛杉矶节点出口（延迟从 249ms 降至 15ms）。

**入口节点**（启用集群，配置 peer）的 JSON 配置：

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
                },
                "mux": {
                    "enabled": true,
                    "concurrency": 8
                }
            }
        ]
    }
}
```

> 其中 `peers[].mux` 为可选优化项：启用后，入口节点到该 peer 的中继连接会复用同一条已认证的 trojan 隧道（基于 smux），省去每次中继的 TCP/TLS 握手开销。对端为 Trojan-Go 服务端时**无需额外配置**即支持（服务端 mux 路径内建）；若对端不支持 mux，入口节点会自动回退为每次中继独立建连，不影响可用性。默认关闭。

**出口节点**（无需配置集群，只需将 peer 密码加入 password 列表）：

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

YAML 入口节点配置示例：

```yaml
run-type: server
local-addr: 0.0.0.0
local-port: 443
remote-addr: 127.0.0.1
remote-port: 80
password:
  - user-password
ssl:
  cert: your_cert.crt
  key: your_key.key
cluster:
  enabled: true
  node-name: singapore-1
  probe-interval: 120
  probe-timeout: 3000
  relay-threshold: 50
  latency-threshold: 100
  targets:
    - 'cidr:149.154.160.0/20'
    - 'cidr:91.108.0.0/16'
  peers:
    - name: la-1
      host: la.example.com
      port: 443
      password: peer-shared-secret
      websocket:
        enabled: true
        host: la.example.com
        path: /ws
      ssl:
        sni: la.example.com
        verify: true
      mux:
        enabled: true
        concurrency: 8
```

**配置字段说明：**

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | `false` | 是否启用集群出口优选 |
| `force_relay` | bool | `false` | 强制将所有出站流量经最快 peer 中继（跳过目标匹配与本地延迟比较） |
| `node_name` | string | `"local"` | 本节点名称，用于路由表标识 |
| `probe_interval` | int | `120` | 探测间隔（秒），最小 60 |
| `probe_timeout` | int | `3000` | 探测超时（毫秒） |
| `relay_threshold` | int | `50` | 中继阈值（毫秒），peer 延迟比本地低超过此值才中继 |
| `latency_threshold` | int | `100` | 延迟触发阈值（毫秒），origin dial RTT 超过此值的目标才纳入探测 |
| `targets` | []string | `[]` | 目标匹配规则，支持 `cidr:`, `domain:`, `ip:`。所有形式都会被探测（`cidr:` 以块内代表地址探测、结果按 CIDR 记录，块内任意 IP 可命中；`domain:` / `ip:` 默认探测 443 端口） |
| `peers` | []object | `[]` | peer 节点列表 |

**Peer 字段说明：**

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 节点名称 |
| `host` | string | 节点地址 |
| `port` | int | 节点端口 |
| `password` | string | Trojan 认证密码 |
| `weight` | int | 权重，用于 RTT 平局裁决：两个 peer 测得相同延迟时选权重高者；0=纯按 RTT 排序 |
| `websocket.enabled` | bool | 是否使用 WebSocket 连接 peer |
| `websocket.host` | string | WebSocket Host 头 |
| `websocket.path` | string | WebSocket 路径 |
| `ssl.sni` | string | TLS SNI（默认使用 host） |
| `ssl.verify` | bool | 是否验证 peer 的 TLS 证书，**默认 `true`**；显式 `false` 才关闭（关闭后中继流量暴露于中间人风险，不推荐） |
| `mux.enabled` | bool | 是否对该 peer 启用隧道多路复用（默认 `false`） |
| `mux.concurrency` | int | 单条 mux 会话的最大并发 stream 数（默认 `8`） |

**工作原理**：

1. Prober 定期探测各 peer 节点到目标的端到端延迟（通过 TCP→TLS→WebSocket→Trojan 通道）
2. 数据路径中 origin dial 延迟超过 `latency_threshold` 的目标会被动态注册到探测列表
3. RouteTable 使用 EWMA 平滑延迟数据，选择最优出口节点
4. 当 peer 延迟比本地低超过 `relay_threshold` 时，连接自动通过该 peer 中继
5. 中继失败时自动回退到本地直连
6. 本地拨号超时（目标疑似被墙）时立即标记本地不可达并紧急探测各 peer，后续连接无需等待下一个探测周期即可切换中继
7. （可选）启用 `peers[].mux` 后，中继连接复用已建立的隧道会话，避免每次中继重复握手

**可靠性保障**：

- **确定性路由**：路由选择基于精确的 `host:port` 匹配（外加确定性的 host 回退与 CIDR 包含回退），结果稳定可复现，不随进程重启或 map 遍历顺序抖动。
- **拨号超时保护**：到 peer 的每次拨号（TCP/TLS/WS 握手、协议头写入）均有超时上限，半开的 peer 节点不会挂死探测循环或中继。
- **紧急回退负缓存**：对「所有 peer 均失败」的目标，短时间（10s）内抑制重复的紧急回退，避免热门宕机目标把每个用户连接放大成 N 次全栈握手。
- **仅失败才判不可达**：慢但成功的本地拨号不会被误判为「本地不可达」，只有真正失败的拨号才触发切换。

**行为说明**：

> **破坏性变更**：peer TLS 现在默认**校验证书**（`ssl.verify` 缺省即为 `true`）。此前该字段缺省时不校验；使用自签证书且未显式配置的节点互联必须显式设置 `"verify": false`（不推荐），或改用受信任证书。

- **首次访问行为**：对尚未探测过的目标，第一条连接仍需经历一次本地拨号；本地拨号超时（疑似被墙）时立即把本地标记为不可达并触发紧急探测，同一条连接会紧急回退到可用 peer，后续连接直接按路由表中继，无需等待下一个探测周期。「慢拨号」判定阈值与 freedom 层的 `dial_timeout` 联动（配置了 `dial_timeout > 0` 时取其值，否则默认 5s），不再是孤立的硬编码。
- **DNS 解析开销**：集群决策路径共享一个 30 秒 TTL 的解析缓存，并合并同一域名的并发解析；单条连接全链路最多触发一次 DNS 解析（匹配、查表、本地拨号复用同一结果），解析结果会写回连接地址供下游拨号直接使用。解析失败或超时不会阻塞，决策路径快速降级为本地直连。

启用连接监控后，可通过 `GET /api/cluster` 查看集群路由状态。

## sing-mux 兼容（Shadowrocket / sing-box Mux 支持）

trojan-go 现在兼容 sing-mux 协议，支持 Shadowrocket 和 sing-box 客户端开启 Mux 多路复用功能。服务端通过 trojan 协议头中的目标地址自动识别客户端类型，**无需额外配置**。

### 支持的 mux 协议

| 协议 | 状态 |
|------|------|
| smux | 已支持 |
| yamux | 已支持 |
| h2mux | 暂未支持 |

### 工作原理

sing-mux 客户端（Shadowrocket / sing-box）连接服务端时，trojan 协议头中目标地址为 `sp.mux.sing-box.arpa:444`。trojan-go 检测到该魔术地址后，走 sing-mux 处理路径：

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

| 参数 | 推荐值 | 说明 |
|------|--------|------|
| `proxy_read_timeout` | `3600s` | 防止空闲断连，建议 ≥ 1h |
| `proxy_send_timeout` | `3600s` | 与 read_timeout 一致 |
| `proxy_connect_timeout` | `60s` | 建连超时，默认即可 |

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

## 软件更新状态

> 最后检查日期：2026-08-31

### 上游项目状态

原版 [p4gefau1t/trojan-go](https://github.com/p4gefau1t/trojan-go) 自 2021 年 9 月发布 v0.10.6 后已停止维护。以下是活跃的社区分支和替代项目：

| 项目 | 状态 | 说明 |
|------|------|------|
| [Potterli20/trojan-go-fork](https://github.com/Potterli20/trojan-go-fork) | ✅ 活跃维护 | 社区维护分支，兼容原版配置，最新发布于 2026 年 6 月 |
| [XTLS/Xray-core](https://github.com/XTLS/Xray-core) | ✅ 活跃维护 | 现代替代方案，支持 Trojan / VLESS / VMess 等多协议，38K+ Stars |
| [SagerNet/sing-box](https://github.com/SagerNet/sing-box) | ✅ 活跃维护 | 通用代理平台，支持 Trojan 等多种协议 |

### Go 版本要求

- **go.mod 指定版本**：`go 1.25.13`
- **说明**：依赖升级（#16）后，`golang.org/x/net` 等库要求较新的工具链；`go 1.25.13` 同时包含标准库安全修复（govulncheck 已验证调用图上无其他已知漏洞）。使用更早的 Go 版本构建时，工具链会按需自动下载。

### 依赖更新概览

关键依赖已全部升级并通过完整测试（#16）：

| 依赖包 | 当前版本 | 说明 |
|--------|----------|------|
| `gopkg.in/yaml.v3` | `v3.0.1` | 修复 CVE-2022-28948（恶意 YAML 触发 panic/DoS），配置解析路径直接受益 |
| `golang.org/x/net` | `v0.58.0` | 覆盖 http2 / html 历年 CVE |
| `golang.org/x/crypto` | `v0.55.0` | |
| `google.golang.org/grpc` | `v1.83.2` | API 服务层 |
| `google.golang.org/protobuf` | `v1.36.12` | |
| `github.com/refraction-networking/utls` | `v1.8.2` | TLS 指纹集合更新至当前主流浏览器；本项目使用的 `UClient`/`HelloXxx_Auto` API 兼容，指纹测试通过 |
| `github.com/go-sql-driver/mysql` | `v1.10.0` | |
| `github.com/v2fly/v2ray-core/v4` | `v4.42.1` | **暂缓**：修复版（v4.44.0+）引入已废弃的 `inet.af/netaddr` 依赖，后者在 Go ≥ 1.22 运行时启动即 panic，无法兼容；残留漏洞 GO-2022-0550 需要恶意 GeoIP 文件才可触发，风险低 |

> **验证**：`govulncheck ./...` 显示调用图上仅剩上述 v2ray-core 暂缓项，其余已知漏洞均已消除。

### 更新命令参考

```shell
# 查看所有可用更新
go list -m -u all | grep '\['

# 更新单个依赖（示例）
go get golang.org/x/crypto@latest
go get golang.org/x/net@latest

# 更新所有依赖（需谨慎，可能引入不兼容）
go get -u ./...

# 整理 go.mod
go mod tidy

# 运行测试验证
make test
```

## 构建

> 请确保 Go 版本 >= 1.25.13（go.mod 要求；更早的 Go 会在构建时自动下载对应工具链）

使用 `make` 进行编译：

```shell
git clone https://github.com/1238616/trojan-go.git
cd trojan-go
make
make install #安装systemd服务等，可选
```

或者使用 Go 自行编译：

```shell
go build -tags "full"
```

> **重要：** 必须指定 `-tags "full"`（或其他功能标签如 `client`、`server`），否则编译出的二进制仅包含 `-version` 标志，不支持 `-config` 等运行时参数。

Go 支持通过设置环境变量进行交叉编译，例如：

编译适用于 64 位 Windows 操作系统的可执行文件：

```shell
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags "full" -trimpath -ldflags="-s -w"
```

编译适用于 Apple Silicon 的可执行文件：

```shell
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -tags "full" -trimpath -ldflags="-s -w"
```

编译适用于 64 位 Linux 操作系统的可执行文件：

```shell
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "full" -trimpath -ldflags="-s -w"
```

## 致谢

- [Trojan](https://github.com/trojan-gfw/trojan)
- [V2Fly](https://github.com/v2fly)
- [utls](https://github.com/refraction-networking/utls)
- [smux](https://github.com/xtaci/smux)
- [go-tproxy](https://github.com/LiamHaworth/go-tproxy)

## Stargazers over time

[![Stargazers over time](https://starchart.cc/p4gefau1t/trojan-go.svg)](https://starchart.cc/p4gefau1t/trojan-go)
