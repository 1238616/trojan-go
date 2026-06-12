//go:build !linux

package connmonitor

import (
	"errors"
	"net"
)

// SocketStats is a placeholder on non-Linux platforms. The fields are
// never populated but the type is defined so cross-platform code can
// reference it without build tags.
type SocketStats struct {
	RTTUs      uint32
	RTOUs      uint32
	SndCwnd    uint32
	BytesAcked uint64
	Loss       uint32
}

// errNilConn is a shared sentinel error.
var errNilConn = errors.New("socket: nil connection")

// ErrSocketNotSupported is returned on non-Linux platforms.
var ErrSocketNotSupported = errors.New("socket: TCP_INFO not supported on this platform")

// SampleTCPInfo always returns ErrSocketNotSupported on non-Linux
// platforms.
func SampleTCPInfo(conn *net.TCPConn) (*SocketStats, error) {
	return nil, ErrSocketNotSupported
}
