# Trojan-Go Linux AMD64 Deployment Guide

## Build Information

| Item | Value |
|------|-------|
| **Target** | linux/amd64 (x86-64) |
| **Binary** | `trojan-go-linux-amd64` |
| **Size** | 16 MB |
| **Type** | ELF 64-bit LSB executable, statically linked, stripped |
| **Go Version** | 1.19 |
| **Base Tag** | v0.10.6 |
| **CGO** | Disabled (fully static, no glibc dependency) |
| **SHA-256** | `81761a4d7fee35c686e0bcebbfc92799eca39b66f5d012a48993f737b02d7c58` |

### Build Command

```shell
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags "full" -trimpath -ldflags="-s -w"
```

### Performance Optimizations in This Build

| Fix | Path | Improvement |
|-----|------|-------------|
| WriteFrameKeep two-write | muxcool upload | 7.4x faster, 99.9% less alloc (9,472→8 B/op) |
| Channel-based data delivery | muxcool download | Eliminates io.Pipe double-copy (one less memcpy per frame) |
| Peek-based frame read | muxcool server | Avoids alloc for small frames via bufio.Reader.Peek |
| ReadFrameMetadata stack array | muxcool frame parse | Eliminated binary.Read interface boxing |
| Splice(2) infrastructure | proxy relay | UnwrapTCPConn + SpliceRelayCounted for zero-copy TCP→TCP |
| strconv.AppendInt | proxy conn ID | Eliminated fmt.Sprintf per-connection alloc |
| Redirector DialTimeout | TCP redirect | 10s dial timeout prevents goroutine leak |

## Quick Deploy

### 1. Upload Binary

```shell
scp trojan-go-linux-amd64 user@server:/usr/bin/trojan-go
ssh user@server 'chmod +x /usr/bin/trojan-go'
```

### 2. Create Config Directory

```shell
ssh user@server 'mkdir -p /etc/trojan-go'
```

### 3. Write Configuration

Upload or create `/etc/trojan-go/config.json` on the server:

```json
{
    "run_type": "server",
    "local_addr": "0.0.0.0",
    "local_port": 443,
    "remote_addr": "127.0.0.1",
    "remote_port": 80,
    "password": ["your_password"],
    "ssl": {
        "cert": "/etc/trojan-go/server.crt",
        "key": "/etc/trojan-go/server.key",
        "sni": "your-domain.com"
    },
    "router": {
        "enabled": true,
        "block": ["geoip:private"],
        "geoip": "/usr/share/trojan-go/geoip.dat",
        "geosite": "/usr/share/trojan-go/geosite.dat"
    },
    "conn_monitor": {
        "enabled": true,
        "addr": "127.0.0.1",
        "port": 9090,
        "secret": "your-dashboard-secret"
    }
}
```

> YAML format is also supported. Use `config.yaml` with hyphenated keys (e.g. `run-type`, `local-addr`).

### 4. Place TLS Certificates

```shell
scp server.crt server.key user@server:/etc/trojan-go/
```

### 5. Place GeoIP/GeoSite Data (if using router)

```shell
scp geoip.dat geosite.dat user@server:/usr/share/trojan-go/
```

## Systemd Service

### Install Service Unit

Create `/etc/systemd/system/trojan-go.service`:

```ini
[Unit]
Description=Trojan-Go - An unidentifiable mechanism that helps you bypass GFW
Documentation=https://p4gfau1t.github.io/trojan-go/
After=network.target nss-lookup.target

[Service]
User=nobody
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ExecStart=/usr/bin/trojan-go -config /etc/trojan-go/config.json
Restart=on-failure
RestartSec=10s
LimitNOFILE=infinity

[Install]
WantedBy=multi-user.target
```

### Enable and Start

```shell
sudo systemctl daemon-reload
sudo systemctl enable trojan-go
sudo systemctl start trojan-go
```

### Verify

```shell
sudo systemctl status trojan-go
journalctl -u trojan-go -f          # live logs
```

## One-Command Deploy Script

```shell
#!/bin/bash
set -euo pipefail

SERVER="user@your-server"
BINARY="trojan-go-linux-amd64"

# Upload binary
scp "$BINARY" "$SERVER:/usr/bin/trojan-go"
ssh "$SERVER" 'chmod +x /usr/bin/trojan-go'

# Upload config and certs
ssh "$SERVER" 'mkdir -p /etc/trojan-go /usr/share/trojan-go'
scp config.json "$SERVER:/etc/trojan-go/config.json"
scp server.crt server.key "$SERVER:/etc/trojan-go/"
scp geoip.dat geosite.dat "$SERVER:/usr/share/trojan-go/"

# Install and start service
scp trojan-go.service "$SERVER:/etc/systemd/system/trojan-go.service"
ssh "$SERVER" <<'EOF'
  sudo systemctl daemon-reload
  sudo systemctl enable trojan-go
  sudo systemctl restart trojan-go
  sleep 2
  sudo systemctl status trojan-go --no-pager
EOF

echo "Deploy complete."
```

## Verification Checklist

| Check | Command | Expected |
|-------|---------|----------|
| Binary runs | `trojan-go -version` | Prints version, exits 0 |
| Port listening | `ss -tlnp \| grep 443` | `LISTEN` on `:443` |
| Monitor API | `curl http://127.0.0.1:9090/api/summary` | JSON with connection stats |
| Dashboard | Browser → `http://server:9090/dashboard` | Web UI loads |
| Prometheus | `curl http://127.0.0.1:9090/metrics` | Prometheus text format |
| TLS handshake | `openssl s_client -connect server:443 -servername your-domain.com` | Certificate chain OK |
| Service status | `systemctl status trojan-go` | `active (running)` |

## Upgrade Procedure

```shell
# Stop service
sudo systemctl stop trojan-go

# Replace binary
sudo cp trojan-go-linux-amd64 /usr/bin/trojan-go
sudo chmod +x /usr/bin/trojan-go

# Restart
sudo systemctl start trojan-go

# Verify
sudo systemctl status trojan-go
```

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `permission denied` on port 443 | Missing `CAP_NET_BIND_SERVICE` | Ensure systemd unit has `AmbientCapabilities` |
| `exec format error` | Wrong architecture binary | Rebuild with correct `GOARCH` |
| `certificate not found` | Wrong cert/key path | Check `ssl.cert` and `ssl.key` in config |
| `geoip.dat not found` | Missing data files | Place files at configured `router.geoip` path |
| High CPU on idle | mux cool busy-loop | This build includes pooled buffer fix — should be resolved |
| Dashboard 401 | Missing auth | Use `?token=<secret>` or `Authorization: Bearer <secret>` |
