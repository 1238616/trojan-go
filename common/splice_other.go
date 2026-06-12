//go:build !linux

package common

import (
	"errors"
	"net"
)

// ErrSpliceNotSupported is returned on platforms where splice(2) is
// not available.
var ErrSpliceNotSupported = errors.New("splice: not supported on this platform")

// SpliceRelay always returns ErrSpliceNotSupported on non-Linux
// platforms. The caller should fall back to copyBuffer.
func SpliceRelay(dst, src net.Conn) (int64, error) {
	return 0, ErrSpliceNotSupported
}

// SpliceRelayCounted always returns ErrSpliceNotSupported on non-Linux
// platforms. The caller should fall back to copyBuffer.
func SpliceRelayCounted(dst, src net.Conn, countFn func(n int64)) (int64, error) {
	return 0, ErrSpliceNotSupported
}
