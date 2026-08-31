package proxy

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// Regression tests for issue #10: UDP flows must be visible in the
// connection monitor, and zero-length datagrams must not terminate flows.

var packetTestSeq atomic.Int64

// scriptedPacketConn is a tunnel.PacketConn that delivers a fixed sequence
// of datagrams from ReadWithMetadata and then blocks like an idle UDP
// socket until Close.
type scriptedPacketConn struct {
	packets   [][]byte        // datagrams delivered in order
	next      int
	target    *tunnel.Address // address reported with each datagram (may be nil)
	written   chan []byte     // copies of datagrams forwarded via WriteWithMetadata
	done      chan struct{}   // closed by Close(), unblocks idle reads
	closeOnce sync.Once
}

func newScriptedPacketConn(target *tunnel.Address, packets ...[]byte) *scriptedPacketConn {
	return &scriptedPacketConn{
		packets: packets,
		target:  target,
		written: make(chan []byte, 16),
		done:    make(chan struct{}),
	}
}

func (c *scriptedPacketConn) ReadWithMetadata(p []byte) (int, *tunnel.Metadata, error) {
	if c.next < len(c.packets) {
		pkt := c.packets[c.next]
		c.next++
		n := copy(p, pkt)
		var md *tunnel.Metadata
		if c.target != nil {
			md = &tunnel.Metadata{Address: c.target}
		}
		return n, md, nil
	}
	<-c.done // idle flow: wait for Close, like a real UDP socket
	return 0, nil, net.ErrClosed
}

func (c *scriptedPacketConn) WriteWithMetadata(p []byte, _ *tunnel.Metadata) (int, error) {
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case c.written <- cp:
	case <-time.After(2 * time.Second):
		return 0, errors.New("test: WriteWithMetadata timed out")
	}
	return len(p), nil
}

func (c *scriptedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, _, err := c.ReadWithMetadata(p)
	return n, nil, err
}

func (c *scriptedPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return c.WriteWithMetadata(p, nil)
}

func (c *scriptedPacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

func (c *scriptedPacketConn) LocalAddr() net.Addr                { return drainAddr{} }
func (c *scriptedPacketConn) SetDeadline(time.Time) error        { return nil }
func (c *scriptedPacketConn) SetReadDeadline(time.Time) error    { return nil }
func (c *scriptedPacketConn) SetWriteDeadline(time.Time) error   { return nil }

func nextPacketFlowID() string {
	return fmt.Sprintf("udp-relay-%d", packetTestSeq.Add(1))
}

// findConnInfo locates one connection in the monitor snapshot.
func findConnInfo(t *testing.T, id string) connmonitor.ConnInfo {
	t.Helper()
	for _, ci := range connmonitor.Global().GetAll() {
		if ci.ID == id {
			return ci
		}
	}
	t.Fatalf("flow %s not present in the connection monitor snapshot", id)
	return connmonitor.ConnInfo{}
}

// TestZeroLengthDatagramKeepsUDPFlowAlive is the acceptance test from
// issue #10: a zero-length datagram (DNS keepalive, QUIC / game probe)
// is a legal UDP payload and must be forwarded without ending the flow.
// Before the fix, copyPacket treated n == 0 as end-of-stream and tore
// the flow down, dropping every subsequent datagram.
func TestZeroLengthDatagramKeepsUDPFlowAlive(t *testing.T) {
	target := tunnel.NewAddressFromHostPort("udp", "8.8.8.8", 53)
	inbound := newScriptedPacketConn(target,
		[]byte("hello"),
		[]byte{}, // zero-length datagram
		[]byte("world"),
	)
	defer inbound.Close()
	outbound := newScriptedPacketConn(nil) // write-only sink
	defer outbound.Close()

	id := nextPacketFlowID()
	entry := connmonitor.Global().RegisterPacketEntry(id, "udp")
	t.Cleanup(func() { connmonitor.Global().UnregisterEntry(entry) })

	errChan := make(chan error, 2)
	go relayPacketDir(inbound, outbound, entry, true, errChan)

	// All three datagrams — including the zero-length one — must arrive.
	want := []string{"hello", "", "world"}
	for i, w := range want {
		select {
		case got := <-outbound.written:
			if string(got) != w {
				t.Fatalf("datagram %d: got %q, want %q", i, got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("datagram %d (%q) was not forwarded within 2s — flow died early", i, w)
		}
	}

	// The flow must still be alive after the zero-length datagram.
	select {
	case err := <-errChan:
		t.Fatalf("flow ended prematurely (err=%v) — zero-length datagram killed it", err)
	default:
	}

	// The monitor must show the flow with type "udp", the byte total,
	// and the target learned from the first packet.
	info := findConnInfo(t, id)
	if info.Type != "udp" {
		t.Fatalf("flow type = %q, want udp", info.Type)
	}
	if info.UploadBytes != int64(len("hello")+len("world")) {
		t.Fatalf("upload bytes = %d, want %d", info.UploadBytes, len("hello")+len("world"))
	}
	if info.Target != "8.8.8.8:53" {
		t.Fatalf("target = %q, want 8.8.8.8:53 learned from the first packet", info.Target)
	}

	// Closing the inbound end must terminate the direction cleanly.
	inbound.Close()
	select {
	case <-errChan:
	case <-time.After(2 * time.Second):
		t.Fatal("relayPacketDir did not exit after the inbound connection closed")
	}
}

// TestRelayPacketDirCountsDownloadBytes covers the response direction:
// bytes are accounted as download, and no target learning happens there.
func TestRelayPacketDirCountsDownloadBytes(t *testing.T) {
	// Real packet connections attach a source address to every datagram.
	inbound := newScriptedPacketConn(tunnel.NewAddressFromHostPort("udp", "9.9.9.9", 8443),
		[]byte("resp-one"), []byte("r2"))
	defer inbound.Close()
	outbound := newScriptedPacketConn(tunnel.NewAddressFromHostPort("udp", "1.1.1.1", 443))
	defer outbound.Close()

	id := nextPacketFlowID()
	entry := connmonitor.Global().RegisterPacketEntry(id, "udp")
	t.Cleanup(func() { connmonitor.Global().UnregisterEntry(entry) })

	errChan := make(chan error, 2)
	go relayPacketDir(inbound, outbound, entry, false, errChan)

	for i := 0; i < 2; i++ {
		select {
		case <-outbound.written:
		case <-time.After(2 * time.Second):
			t.Fatalf("download datagram %d was not forwarded within 2s", i)
		}
	}

	info := findConnInfo(t, id)
	if info.Type != "udp" {
		t.Fatalf("flow type = %q, want udp", info.Type)
	}
	if info.DownloadBytes != int64(len("resp-one")+len("r2")) {
		t.Fatalf("download bytes = %d, want %d", info.DownloadBytes, len("resp-one")+len("r2"))
	}
	// Only the upload direction learns the target.
	if info.Target != "udp" {
		t.Fatalf("target = %q, want the registration placeholder udp", info.Target)
	}

	inbound.Close()
	select {
	case <-errChan:
	case <-time.After(2 * time.Second):
		t.Fatal("relayPacketDir did not exit after the inbound connection closed")
	}
}
