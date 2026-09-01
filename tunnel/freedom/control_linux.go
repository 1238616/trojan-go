//go:build linux
// +build linux

package freedom

import (
	"syscall"
)

// controlHook is the net.Dialer.Control function for Linux.
// It sets socket buffer sizes and enables TCP_QUICKACK to reduce
// delayed-ACK latency on high-speed proxy links.
func (c *Client) controlHook(network, address string, conn syscall.RawConn) error {
	var opErr error
	conn.Control(func(fd uintptr) {
		if c.readBuffer > 0 {
			opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, c.readBuffer)
			if opErr != nil {
				return
			}
		}
		if c.writeBuffer > 0 {
			opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, c.writeBuffer)
			if opErr != nil {
				return
			}
		}
		// TCP_NODELAY: Go's SetNoDelay will also set this after connect,
		// but setting it here ensures it's active during the handshake.
		opErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		if opErr != nil {
			return
		}
		// TCP_QUICKACK (Linux only): reduce delayed ACK latency.
		// The kernel may not support this on all versions; ignore errors.
		const tcpQuickack = 0x0c
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpQuickack, 1)
	})
	return opErr
}

// applyKeepalive sets aggressive TCP keepalive parameters on an established
// connection. Linux only; other platforms get a no-op (keepalive_other.go).
// Errors are ignored: keepalive tuning is best-effort and the constants are
// valid on every Linux kernel trojan-go supports.
func (c *Client) applyKeepalive(conn syscall.RawConn) {
	conn.Control(func(fd uintptr) {
		if c.keepIdleSec > 0 {
			syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, c.keepIdleSec)
		}
		if c.keepIntvlSec > 0 {
			syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, c.keepIntvlSec)
		}
		if c.keepCnt > 0 {
			syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, c.keepCnt)
		}
	})
}
