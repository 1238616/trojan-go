package muxcool

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

// MuxCoolConn represents a single mux.cool session as a tunnel.Conn.
// Read gets data from the frame dispatcher via a channel; Write sends
// Keep frames back.
//
// Channel-based delivery eliminates the double-copy that the previous
// io.Pipe approach incurred on the download path:
//
//	old: ReadDataFrame→alloc → io.Pipe.Write→internal copy → io.Pipe.Read→copy to caller
//	new: ReadDataFrame→alloc → chan send → copy to caller (one less copy)
type MuxCoolConn struct {
	id       uint16
	readCh   chan []byte // data frames from the dispatcher
	readBuf  []byte      // leftover data from a partially consumed frame
	writeMu  *sync.Mutex // shared lock for the underlying connection write
	writer   io.Writer   // underlying connection (shared across sessions)
	underlay tunnel.Conn // the trojan conn (for RemoteAddr, etc.)
	metadata *tunnel.Metadata
	closed   bool
	closeMu  sync.Mutex
}

func newMuxCoolConn(id uint16, target *tunnel.Address, writer io.Writer, writeMu *sync.Mutex, underlay tunnel.Conn) *MuxCoolConn {
	return &MuxCoolConn{
		id:       id,
		readCh:   make(chan []byte, 64), // buffered to avoid blocking the dispatcher
		writeMu:  writeMu,
		writer:   writer,
		underlay: underlay,
		metadata: &tunnel.Metadata{
			Command: 1, // Connect
			Address: target,
		},
	}
}

func (c *MuxCoolConn) Read(p []byte) (int, error) {
	// Serve leftover data from a previous frame first.
	if len(c.readBuf) > 0 {
		n := copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		if len(c.readBuf) == 0 {
			c.readBuf = nil // release reference for GC
		}
		return n, nil
	}
	// Wait for the next frame from the dispatcher.
	data, ok := <-c.readCh
	if !ok {
		return 0, io.EOF // closeRead() closed the channel
	}
	n := copy(p, data)
	if n < len(data) {
		c.readBuf = data[n:]
	}
	return n, nil
}

func (c *MuxCoolConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// mux.cool uses uint16 for data length, so max is 65535.
	// Using 32KB chunks reduces syscall overhead vs 8KB while
	// staying well within the protocol limit.
	const maxChunk = 32 * 1024
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxChunk {
			chunk = p[:maxChunk]
		}
		if err := WriteFrameKeep(c.writer, c.id, chunk); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

func (c *MuxCoolConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true

	// Signal EOF to any pending Read.
	close(c.readCh)

	c.writeMu.Lock()
	WriteFrameEnd(c.writer, c.id)
	c.writeMu.Unlock()

	return nil
}

// feedData is called by the frame dispatcher to push data into this session.
// The data slice is sent through the channel; the receiver owns it after send.
func (c *MuxCoolConn) feedData(data []byte) error {
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()
	if closed {
		return io.ErrClosedPipe
	}
	select {
	case c.readCh <- data:
	default:
		// Channel full — block outside the lock to avoid deadlock
		// with Close() which needs closeMu.
		c.readCh <- data
	}
	return nil
}

// closeRead is called when the remote sends End for this session.
func (c *MuxCoolConn) closeRead() {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.readCh)
	}
}

func (c *MuxCoolConn) Metadata() *tunnel.Metadata {
	return c.metadata
}

func (c *MuxCoolConn) LocalAddr() net.Addr {
	return c.underlay.LocalAddr()
}

func (c *MuxCoolConn) RemoteAddr() net.Addr {
	return c.underlay.RemoteAddr()
}

func (c *MuxCoolConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *MuxCoolConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *MuxCoolConn) SetWriteDeadline(t time.Time) error {
	return nil
}
