//go:build !linux
// +build !linux

package freedom

import "syscall"

// applyKeepalive is a no-op outside Linux: the aggressive keepalive tuning
// uses Linux-only socket options (see control_linux.go).
func (c *Client) applyKeepalive(conn syscall.RawConn) {}
