package proxy

import (
	"io"
	"sync"

	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

// relayBufferSize is the default buffer size used by copyBuffer for TCP
// relays. 32 KiB matches the default io.Copy uses and is a reasonable
// trade-off between per-call syscall overhead and memory footprint.
//
// The value can be overridden at startup via SetRelayBufferSize, which
// must be called before any getBuf() invocations.
const defaultRelayBufferSize = 32 * 1024

// minRelayBufferSize / maxRelayBufferSize are hard safety bounds applied
// by SetRelayBufferSize; values outside this range are clamped to keep
// the proxy well behaved even when the config is mis-typed.
const (
	minRelayBufferSize = 4 * 1024    // 4 KiB
	maxRelayBufferSize = 1024 * 1024 // 1 MiB
)

// relayBuffer is a process-wide buffer size used by every TCP relay. It
// is initialised to defaultRelayBufferSize and may be raised (or clamped)
// via SetRelayBufferSize. Reads are safe because the value is written
// once during config loading and then only read from the hot path.
var relayBufferSz uint32 = defaultRelayBufferSize

// SetRelayBufferSize updates the shared relay buffer size. It is the
// caller's responsibility to invoke this during proxy startup, before
// any relay goroutines are spawned. Values outside [min, max] are
// clamped and the post-clamp value is returned.
func SetRelayBufferSize(n int) int {
	if n < minRelayBufferSize {
		n = minRelayBufferSize
	}
	if n > maxRelayBufferSize {
		n = maxRelayBufferSize
	}
	// align to 1 KiB to avoid accidental odd sizes
	n = (n + 1023) &^ 1023
	relayBufferSz = uint32(n)
	return n
}

// RelayBufferSize returns the active per-relay buffer size in bytes.
func RelayBufferSize() int { return int(relayBufferSz) }

// tcpPoolStats is the process-global PoolStats sink for the TCP relay
// buffer pool. Registered once at import time so the dashboard can
// display the hit-rate even before the first Get().
var tcpPoolStats = connmonitor.RegisterPool("tcp_relay")

// pooledBuffer is one sync.Pool element: the buffer plus a marker
// recording whether pool.New produced it. Counting the miss at Get time
// (exactly once, in getBuf) instead of inside New fixes the double
// count where one miss was recorded as two Gets and the hit-rate was
// systematically inflated (issue #8). The marker is cleared on Get, so
// a buffer that survives a Put/Get cycle counts as a hit.
type pooledBuffer struct {
	buf   []byte
	fresh bool
}

// bufferPool is the sync.Pool backing TCP relay buffers.
var bufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, defaultRelayBufferSize)
		return &pooledBuffer{buf: b, fresh: true}
	},
}

// getBuf borrows a relay buffer from the pool. The returned slice length
// reflects the pool's last-known size; callers who raised
// RelayBufferSize at runtime may see a smaller slice until the pool
// churns. The slice is re-sliced to the active RelayBufferSize() so that
// copyBuffer uses exactly the configured amount.
func getBuf() *pooledBuffer {
	pb := bufferPool.Get().(*pooledBuffer)
	// Exactly one count per Get: fresh == true means pool.New ran (miss).
	tcpPoolStats.OnGet(pb.fresh)
	pb.fresh = false
	active := RelayBufferSize()
	if len(pb.buf) < active {
		// Pool element is stale (from before a resize); grow it.
		pb.buf = make([]byte, active)
	} else {
		pb.buf = pb.buf[:active]
	}
	return pb
}

// putBuf returns a relay buffer to the pool.
func putBuf(b *pooledBuffer) {
	if b == nil {
		return
	}
	tcpPoolStats.OnPut()
	bufferPool.Put(b)
}

// ---- Packet (UDP) buffer pool ----

// MaxPacketSize caps the maximum datagram size the packet relay will
// accept. It is the same constant used historically for copyPacket's
// per-flow allocation.
const MaxPacketSize = 1024 * 8

// packetPoolStats is the process-global PoolStats sink for UDP packet
// buffers. The dashboard renders this in the "Pool & UDP" card so that
// hit-rate regressions become visible before they surface as GC pauses.
var packetPoolStats = connmonitor.RegisterPool("udp_packet")

// packetBufferPool recycles MaxPacketSize byte slices used by the UDP
// packet relay. A sync.Pool avoids the per-flow 8 KiB allocation that
// the legacy implementation performed in the hot path.
var packetBufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, MaxPacketSize)
		return &pooledBuffer{buf: b, fresh: true}
	},
}

// getPacketBuf borrows an 8 KiB packet buffer from the pool.
func getPacketBuf() *pooledBuffer {
	pb := packetBufferPool.Get().(*pooledBuffer)
	packetPoolStats.OnGet(pb.fresh) // exactly one count per Get (issue #8)
	pb.fresh = false
	return pb
}

// putPacketBuf returns a packet buffer to the pool.
func putPacketBuf(b *pooledBuffer) {
	if b == nil {
		return
	}
	packetPoolStats.OnPut()
	packetBufferPool.Put(b)
}

// copyBuffer is a simplified io.CopyBuffer that always uses the supplied
// buffer and never tries to use the WriterTo / ReaderFrom shortcuts. It is
// the right primitive for the relay loop because:
//
//   - We deliberately wrap src in a counting reader, which would otherwise
//     defeat io.Copy's automatic splice / sendfile fast path silently.
//   - Reusing a pooled buffer lets long-lived flows operate with zero
//     per-byte allocations.
func copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	var written int64
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = io.ErrShortWrite
				}
			}
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}
