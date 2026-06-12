//go:build linux

package connmonitor

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// errNilConn is a shared sentinel error.
var errNilConn = errors.New("socket: nil connection")

// SocketStats holds a snapshot of TCP socket telemetry obtained via
// getsockopt(TCP_INFO). All values are in kernel-native units.
type SocketStats struct {
	RTTUs      uint32 // smoothed round-trip time in microseconds
	RTOUs      uint32 // retransmission timeout in microseconds
	SndCwnd    uint32 // send congestion window in segments
	BytesAcked uint64 // total bytes acknowledged
	Loss       uint32 // retransmit/reorder loss counter
}

// SampleTCPInfo retrieves TCP_INFO from the given connection's
// underlying file descriptor. Returns nil and an error when the
// connection does not expose a SyscallConn.
func SampleTCPInfo(conn *net.TCPConn) (*SocketStats, error) {
	if conn == nil {
		return nil, errNilConn
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var stats SocketStats
	var sysErr error
	err = raw.Control(func(fd uintptr) {
		info, e := unix.GetsockoptTCPInfo(int(fd), syscall.IPPROTO_TCP, syscall.TCP_INFO)
		if e != nil {
			sysErr = e
			return
		}
		stats = SocketStats{
			RTTUs:   info.Rtt,
			RTOUs:   info.Rto,
			SndCwnd: info.Snd_cwnd,
			Loss:    info.Total_retrans,
		}
	})
	if err != nil {
		return nil, err
	}
	return &stats, sysErr
}
