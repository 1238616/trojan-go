package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel/freedom"
)

// Regression tests for issue #1: one-direction-only endings must not
// leave the relay goroutines blocked until TCP keepalive fires.

var drainTestSeq atomic.Int64

// scriptedConn is a net.Conn whose Read behavior is fixed up front, for
// the relay-drain tests. Write succeeds (discards) until Close.
type scriptedConn struct {
	readErr error // returned by every Read (io.EOF = clean end)
	closed  atomic.Bool
}

func (c *scriptedConn) Read(p []byte) (int, error) { return 0, c.readErr }
func (c *scriptedConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return len(p), nil
}
func (c *scriptedConn) Close() error                     { c.closed.Store(true); return nil }
func (c *scriptedConn) LocalAddr() net.Addr              { return drainAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr             { return drainAddr{} }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

type drainAddr struct{}

func (drainAddr) Network() string { return "drain-test" }
func (drainAddr) String() string  { return "drain-test" }

// runRelay runs relayBidirectional and fails the test if it does not
// terminate on its own within 5 seconds — which is exactly the bug in
// issue #1 (relays hanging ~60s until TCP keepalive).
func runRelay(t *testing.T, inbound, outbound net.Conn) connmonitor.CloseReason {
	t.Helper()
	id := fmt.Sprintf("relay-drain-%d", drainTestSeq.Add(1))
	entry := connmonitor.Global().RegisterEntry(id, "drain.test:443")
	t.Cleanup(func() { connmonitor.Global().UnregisterEntry(entry) })

	done := make(chan connmonitor.CloseReason, 1)
	go func() {
		done <- relayBidirectional(context.Background(), inbound, outbound,
			entry, connmonitor.GlobalMetrics(), time.Now(), id, "drain.test:443")
	}()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not terminate within 5s; goroutines would hang until TCP keepalive")
		return connmonitor.CloseReasonOther
	}
}

// waitForGoroutines polls until the goroutine count drops back to the
// recorded baseline, proving neither copy goroutine lingered.
func waitForGoroutines(t *testing.T, baseline int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: goroutine count did not return to baseline %d (now %d) — relay goroutines leaked",
		what, baseline, runtime.NumGoroutine())
}

// TestRelayPropagatesUploadEOFAndDrains covers the exact scenario from
// issue #1: the client half-closes the upload, the relay must forward
// that EOF to the origin via CloseWrite so the origin can finish its
// response, and both copy goroutines must exit without keepalive.
func TestRelayPropagatesUploadEOFAndDrains(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// A minimal origin: waits for the request-side EOF, then answers.
	// Before the fix it never saw that EOF and the download direction
	// blocked forever.
	originSawEOF := make(chan struct{})
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		if _, err := io.Copy(io.Discard, conn); err != nil {
			return
		}
		close(originSawEOF)
		conn.Write([]byte("pong"))
	}()

	tcpConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial origin: %v", err)
	}
	outbound := &freedom.Conn{Conn: tcpConn}
	// Client already half-closed the upload: Read yields EOF at once.
	inbound := &scriptedConn{readErr: io.EOF}

	g0 := runtime.NumGoroutine()
	reason := runRelay(t, inbound, outbound)
	if reason != connmonitor.CloseReasonEOF {
		t.Fatalf("expected EOF close reason, got %v", reason)
	}
	select {
	case <-originSawEOF:
	default:
		t.Fatal("origin never observed the upload EOF — half-close was not propagated")
	}
	waitForGoroutines(t, g0, "half-close relay")
}

// TestRelayClosesBothEndsOnUploadError asserts that a hard error in one
// direction closes both ends immediately so the other goroutine is
// unblocked instead of lingering on Read.
func TestRelayClosesBothEndsOnUploadError(t *testing.T) {
	outA, outB := net.Pipe()
	defer outB.Close()
	inbound := &scriptedConn{readErr: io.ErrUnexpectedEOF}

	reason := runRelay(t, inbound, outA)
	if reason == connmonitor.CloseReasonEOF {
		t.Fatalf("expected non-EOF close reason for upload error, got %v", reason)
	}
	if !inbound.closed.Load() {
		t.Fatal("inbound was not closed after the upload error")
	}
	// outbound must have been closed to wake the download goroutine.
	buf := make([]byte, 8)
	outB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := outB.Read(buf); err == nil {
		t.Fatal("expected an error reading the remote half of the closed outbound")
	}
}

// TestRelayClosesBothEndsWhenDownloadEOFFirst covers the origin-hangup
// direction: a clean EOF that cannot be half-closed further (net.Pipe
// has no CloseWrite) must still close both ends and wake the upload
// goroutine.
func TestRelayClosesBothEndsWhenDownloadEOFFirst(t *testing.T) {
	inA, inB := net.Pipe()
	defer inB.Close()
	outbound := &scriptedConn{readErr: io.EOF} // origin hung up

	reason := runRelay(t, inA, outbound)
	if reason != connmonitor.CloseReasonEOF {
		t.Fatalf("expected EOF close reason, got %v", reason)
	}
	if !outbound.closed.Load() {
		t.Fatal("outbound was not closed after the download EOF")
	}
	// The blocked upload goroutine must have been woken by the close.
	buf := make([]byte, 8)
	inB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := inB.Read(buf); err == nil {
		t.Fatal("expected an error reading the remote half of the closed inbound")
	}
}
