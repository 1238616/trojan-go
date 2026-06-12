//go:build !linux
// +build !linux

package freedom

import (
	"syscall"
)

// controlHook is the net.Dialer.Control function for non-Linux platforms.
// It sets socket buffer sizes when configured. TCP_QUICKACK is Linux-only
// and not available here.
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
		opErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
	})
	return opErr
}
