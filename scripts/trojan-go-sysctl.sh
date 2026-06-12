#!/bin/bash
# scripts/trojan-go-sysctl.sh
# Linux kernel TCP/UDP tuning for trojan-go proxy workloads.
# Run as root before starting trojan-go, or add to systemd unit.
#
# Usage:
#   sudo bash scripts/trojan-go-sysctl.sh
#   # or wrap trojan-go startup:
#   sudo bash scripts/trojan-go-sysctl.sh && ./trojan-go -config config.json

set -e

echo "[trojan-go-sysctl] Applying kernel tuning..."

# --- TCP Congestion Control ---
if modprobe tcp_bbr 2>/dev/null; then
    sysctl -w net.ipv4.tcp_congestion_control=bbr
    echo "[trojan-go-sysctl] BBR congestion control enabled"
else
    echo "[trojan-go-sysctl] BBR not available, keeping cubic"
fi

# --- TCP Buffers ---
sysctl -w net.core.rmem_max=16777216     # 16 MB
sysctl -w net.core.wmem_max=16777216     # 16 MB
sysctl -w net.core.rmem_default=1048576  # 1 MB
sysctl -w net.core.wmem_default=1048576  # 1 MB
sysctl -w net.ipv4.tcp_rmem='4096 1048576 16777216'
sysctl -w net.ipv4.tcp_wmem='4096 1048576 16777216'
echo "[trojan-go-sysctl] TCP buffer sizes tuned"

# --- TCP Connection Parameters ---
sysctl -w net.ipv4.tcp_fastopen=3        # client + server TFO
sysctl -w net.ipv4.tcp_keepalive_time=30
sysctl -w net.ipv4.tcp_keepalive_intvl=10
sysctl -w net.ipv4.tcp_keepalive_probes=3
sysctl -w net.ipv4.tcp_fin_timeout=15
echo "[trojan-go-sysctl] TCP connection params tuned"

# --- Listen / Backlog ---
sysctl -w net.core.somaxconn=65535
sysctl -w net.ipv4.tcp_max_syn_backlog=65535
sysctl -w net.core.netdev_max_backlog=65535
echo "[trojan-go-sysctl] Backlog tuned"

# --- MTU ---
sysctl -w net.ipv4.tcp_mtu_probing=2     # always probe
echo "[trojan-go-sysctl] MTU probing enabled"

# --- UDP ---
sysctl -w net.ipv4.udp_rmem_min=8192
sysctl -w net.ipv4.udp_wmem_min=8192
sysctl -w net.ipv4.udp_mem='65536 131072 262144'
echo "[trojan-go-sysctl] UDP params tuned"

# --- File Descriptors ---
ulimit -n 65535
echo "[trojan-go-sysctl] File descriptor limit set to 65535"

echo "[trojan-go-sysctl] All kernel tuning applied successfully"
