package mux

import (
	"io"
	"math/rand"
	"sync"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// stickyBufPool reuses the small coalescing buffer used by stickToPayload
// when there is at least one queued SYN/FIN header. Hot benchmarks show
// per-Write allocations were a measurable cost under high mux fan-in.
var stickyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 1024)
		return &b
	},
}

type stickyConn struct {
	tunnel.Conn
	synQueue chan [8]byte
	finQueue chan [8]byte
}

// stickToPayload coalesces any pending SYN/FIN headers around the payload p.
// The fast path avoids allocating altogether when both queues are empty,
// which is the common case during a steady-state data transfer.
//
// The second return value is the pooled buffer (may be nil); when non-nil
// the caller MUST hand it to putStickyBuf after the Write completes so the
// backing array can be reused.
func (c *stickyConn) stickToPayload(p []byte) ([]byte, *[]byte) {
	if len(c.synQueue) == 0 && len(c.finQueue) == 0 {
		return p, nil
	}
	bufPtr := stickyBufPool.Get().(*[]byte)
	buf := (*bufPtr)[:0]
	for {
		select {
		case header := <-c.synQueue:
			buf = append(buf, header[:]...)
		default:
			goto stick1
		}
	}
stick1:
	buf = append(buf, p...)
	for {
		select {
		case header := <-c.finQueue:
			buf = append(buf, header[:]...)
		default:
			goto stick2
		}
	}
stick2:
	*bufPtr = buf
	return buf, bufPtr
}

// putStickyBuf returns a previously borrowed sticky buffer to the pool.
func putStickyBuf(bp *[]byte) {
	if bp == nil {
		return
	}
	*bp = (*bp)[:0]
	stickyBufPool.Put(bp)
}

func (c *stickyConn) Close() error {
	const maxPaddingLength = 512
	padding := [maxPaddingLength + 8]byte{'A', 'B', 'C', 'D', 'E', 'F'} // for debugging
	buf, bp := c.stickToPayload(nil)
	c.Write(append(buf, padding[:rand.Intn(maxPaddingLength)]...))
	putStickyBuf(bp)
	return c.Conn.Close()
}

func (c *stickyConn) Write(p []byte) (int, error) {
	if len(p) == 8 {
		if p[0] == 1 || p[0] == 2 { // smux 8 bytes header
			switch p[1] {
			// THE CONTENT OF THE BUFFER MIGHT CHANGE
			// COPY THE HEADER VALUE INTO A FIXED-SIZE ARRAY
			case 0:
				// cmdSYN
				var header [8]byte
				copy(header[:], p)
				select {
				case c.synQueue <- header:
				default:
					goto directWrite
				}
				return 8, nil
			case 1:
				// cmdFIN
				var header [8]byte
				copy(header[:], p)
				select {
				case c.finQueue <- header:
				default:
					goto directWrite
				}
				return 8, nil
			}
		} else {
			log.Debug("other 8 bytes header")
		}
	}
directWrite:
	buf, bp := c.stickToPayload(p)
	_, err := c.Conn.Write(buf)
	putStickyBuf(bp)
	return len(p), err
}

func newStickyConn(conn tunnel.Conn) *stickyConn {
	return &stickyConn{
		Conn:     conn,
		synQueue: make(chan [8]byte, 128),
		finQueue: make(chan [8]byte, 128),
	}
}

type Conn struct {
	rwc io.ReadWriteCloser
	tunnel.Conn

	// Phase 3: when non-nil, Close decrements the mux stream gauge.
	metrics *connmonitor.Metrics
}

func (c *Conn) Read(p []byte) (int, error) {
	return c.rwc.Read(p)
}

func (c *Conn) Write(p []byte) (int, error) {
	return c.rwc.Write(p)
}

func (c *Conn) Close() error {
	if c.metrics != nil {
		c.metrics.RecordMuxStreamClose()
	}
	return c.rwc.Close()
}
