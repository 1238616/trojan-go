//go:build linux

package common

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// SpliceBufferSize is the default pipe buffer size used by the splice
// fast path. 64 KiB matches the Linux default pipe buffer and gives a
// good tradeoff between syscall frequency and memory overhead.
const SpliceBufferSize = 64 * 1024

// ErrSpliceNotSupported is returned when splice cannot be used for the
// given connection pair (e.g. one side is not a *net.TCPConn).
var ErrSpliceNotSupported = errors.New("splice: not supported for this connection pair")

// unwrapper is implemented by connections that wrap a *net.TCPConn
// (e.g. freedom.Conn). It allows splice to extract the raw FD.
type unwrapper interface {
	UnwrapTCPConn() *net.TCPConn
}

// spliceFD extracts the raw file descriptor from a net.Conn. It tries
// direct *net.TCPConn first, then checks for the UnwrapTCPConn interface
// to handle wrapped connections (e.g. freedom.Conn). Returns -1 and
// ErrSpliceNotSupported when neither path yields a usable FD.
func spliceFD(c net.Conn) (int, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		if u, ok := c.(unwrapper); ok {
			tc = u.UnwrapTCPConn()
		}
		if tc == nil {
			return -1, ErrSpliceNotSupported
		}
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var fd int
	var sysErr error
	err = raw.Control(func(f uintptr) {
		fd = int(f)
	})
	if err != nil {
		return -1, err
	}
	if sysErr != nil {
		return -1, sysErr
	}
	return fd, nil
}

// SpliceRelay copies data from src to dst using the Linux splice(2)
// syscall when both sides are plain TCP connections. This avoids the
// user-space read/write round trip and lets the kernel move data
// directly between socket buffers via a pipe.
//
// When either side is not a *net.TCPConn (TLS, mux, counting wrappers,
// etc.) SpliceRelay returns ErrSpliceNotSupported so the caller can
// fall back to copyBuffer.
// SpliceRelayCounted is like SpliceRelay but also invokes countFn with
// the number of bytes transferred after each successful splice drain.
// This allows the caller to maintain byte accounting (upload/download
// metrics, TTFB) that would otherwise be lost when splice bypasses the
// countingReader wrapper.
func SpliceRelayCounted(dst, src net.Conn, countFn func(n int64)) (int64, error) {
	srcFD, err := spliceFD(src)
	if err != nil {
		return 0, ErrSpliceNotSupported
	}
	dstFD, err := spliceFD(dst)
	if err != nil {
		return 0, ErrSpliceNotSupported
	}

	pipeFD := []int{0, 0}
	if err := syscall.Pipe(pipeFD); err != nil {
		return 0, err
	}
	pipeR := os.NewFile(uintptr(pipeFD[0]), "splice-r")
	pipeW := os.NewFile(uintptr(pipeFD[1]), "splice-w")
	defer pipeR.Close()
	defer pipeW.Close()

	var written int64
	for {
		n, err := unix.Splice(srcFD, nil, pipeFD[1], nil, SpliceBufferSize, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return written, err
		}
		if n == 0 {
			return written, io.EOF
		}

		var remaining int64 = n
		for remaining > 0 {
			m, err := unix.Splice(pipeFD[0], nil, dstFD, nil, int(remaining), 0)
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				return written, err
			}
			remaining -= int64(m)
			written += int64(m)
			if countFn != nil {
				countFn(int64(m))
			}
		}
	}
}

func SpliceRelay(dst, src net.Conn) (int64, error) {
	srcFD, err := spliceFD(src)
	if err != nil {
		return 0, ErrSpliceNotSupported
	}
	dstFD, err := spliceFD(dst)
	if err != nil {
		return 0, ErrSpliceNotSupported
	}

	// Create a pipe to use as the splice intermediary.
	pipeFD := []int{0, 0}
	if err := syscall.Pipe(pipeFD); err != nil {
		return 0, err
	}
	pipeR := os.NewFile(uintptr(pipeFD[0]), "splice-r")
	pipeW := os.NewFile(uintptr(pipeFD[1]), "splice-w")
	defer pipeR.Close()
	defer pipeW.Close()

	var written int64
	for {
		// splice from src -> pipe write end
		n, err := unix.Splice(srcFD, nil, pipeFD[1], nil, SpliceBufferSize, 0)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return written, err
		}
		if n == 0 {
			return written, io.EOF
		}

		// splice from pipe read end -> dst
		var remaining int64 = n
		for remaining > 0 {
			m, err := unix.Splice(pipeFD[0], nil, dstFD, nil, int(remaining), 0)
			if err != nil {
				if errors.Is(err, syscall.EINTR) {
					continue
				}
				return written, err
			}
			remaining -= int64(m)
			written += int64(m)
		}
	}
}
