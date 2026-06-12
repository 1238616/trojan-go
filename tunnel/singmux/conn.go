package singmux

import (
	"io"
	"net"
	"sync"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

// SingMuxConn wraps a mux stream as a tunnel.Conn.
// Read/Write/Close proxy to the stream; other net.Conn methods
// (LocalAddr, RemoteAddr, deadlines) inherit from the underlying trojan conn.
type SingMuxConn struct {
	rwc io.ReadWriteCloser
	tunnel.Conn
	metadata *tunnel.Metadata
}

func (c *SingMuxConn) Read(p []byte) (int, error)  { return c.rwc.Read(p) }
func (c *SingMuxConn) Write(p []byte) (int, error) { return c.rwc.Write(p) }
func (c *SingMuxConn) Close() error                { return c.rwc.Close() }

func (c *SingMuxConn) Metadata() *tunnel.Metadata {
	return c.metadata
}

// responseStream wraps a net.Conn (mux stream) to automatically prepend
// a status=0 (success) byte on the first Write, matching the sing-mux
// StreamResponse protocol.
type responseStream struct {
	net.Conn
	mu           sync.Mutex
	responseSent bool
}

func newResponseStream(conn net.Conn) *responseStream {
	return &responseStream{Conn: conn}
}

func (r *responseStream) Write(p []byte) (int, error) {
	r.mu.Lock()
	if r.responseSent {
		r.mu.Unlock()
		return r.Conn.Write(p)
	}
	r.responseSent = true
	r.mu.Unlock()

	buf := make([]byte, 1+len(p))
	buf[0] = statusSuccess
	copy(buf[1:], p)
	n, err := r.Conn.Write(buf)
	if n > 0 {
		n--
	}
	if err != nil {
		return n, err
	}
	return len(p), nil
}
