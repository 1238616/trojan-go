//go:build windows
// +build windows

package freedom

import (
	"syscall"
)

// controlHook is the net.Dialer.Control function for Windows.
// It mirrors control_other.go, but Windows socket options take a
// syscall.Handle instead of an int file descriptor (issue #14: the shared
// implementation used to break Windows cross-compilation).
func (c *Client) controlHook(network, address string, conn syscall.RawConn) error {
	var opErr error
	conn.Control(func(fd uintptr) {
		handle := syscall.Handle(fd)
		if c.readBuffer > 0 {
			opErr = syscall.SetsockoptInt(handle, syscall.SOL_SOCKET, syscall.SO_RCVBUF, c.readBuffer)
			if opErr != nil {
				return
			}
		}
		if c.writeBuffer > 0 {
			opErr = syscall.SetsockoptInt(handle, syscall.SOL_SOCKET, syscall.SO_SNDBUF, c.writeBuffer)
			if opErr != nil {
				return
			}
		}
		opErr = syscall.SetsockoptInt(handle, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
	})
	return opErr
}
