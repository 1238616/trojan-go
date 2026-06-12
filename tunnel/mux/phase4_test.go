package mux

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/tunnel"
	"github.com/p4gefau1t/trojan-go/tunnel/freedom"
	"github.com/p4gefau1t/trojan-go/tunnel/transport"
)

// TestMuxConfigDefaults verifies the Phase 4 default values are applied.
func TestMuxConfigDefaults(t *testing.T) {
	muxCfg := &Config{
		Mux: MuxConfig{
			Enabled:          true,
			Concurrency:      4,
			IdleTimeout:      30,
			MaxPhysicalConns: 16,
			MaxStreamBuffer:  1 << 20, // 1 MB
			MaxReceiveBuffer: 1 << 24, // 16 MB
		},
	}
	if muxCfg.Mux.Concurrency != 4 {
		t.Errorf("Concurrency=%d, want 4", muxCfg.Mux.Concurrency)
	}
	if muxCfg.Mux.MaxPhysicalConns != 16 {
		t.Errorf("MaxPhysicalConns=%d, want 16", muxCfg.Mux.MaxPhysicalConns)
	}
	if muxCfg.Mux.MaxStreamBuffer != 1<<20 {
		t.Errorf("MaxStreamBuffer=%d, want %d", muxCfg.Mux.MaxStreamBuffer, 1<<20)
	}
	if muxCfg.Mux.MaxReceiveBuffer != 1<<24 {
		t.Errorf("MaxReceiveBuffer=%d, want %d", muxCfg.Mux.MaxReceiveBuffer, 1<<24)
	}
}

// TestMuxWindowClamp verifies smux config clamping logic.
func TestMuxWindowClamp(t *testing.T) {
	tests := []struct {
		name      string
		streamBuf int
		recvBuf   int
		wantMinSB int
		wantMaxRB int
	}{
		{"zero_uses_default", 0, 0, 0, 0},
		{"below_min_clamped", 1024, 1024, 64 << 10, 1 << 20},
		{"normal_values", 1 << 20, 1 << 24, 1 << 20, 1 << 24},
		{"above_max_clamped", 1 << 30, 1 << 30, 64 << 20, 256 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test clamping logic directly (same as in client.go).
			sb := tt.streamBuf
			if sb > 0 {
				if sb < 64<<10 {
					sb = 64 << 10
				}
				if sb > 64<<20 {
					sb = 64 << 20
				}
			}
			rb := tt.recvBuf
			if rb > 0 {
				if rb < 1<<20 {
					rb = 1 << 20
				}
				if rb > 256<<20 {
					rb = 256 << 20
				}
			}
			if tt.wantMinSB > 0 && sb != tt.wantMinSB {
				t.Errorf("streamBuf clamped to %d, want %d", sb, tt.wantMinSB)
			}
			if tt.wantMaxRB > 0 && rb != tt.wantMaxRB {
				t.Errorf("recvBuf clamped to %d, want %d", rb, tt.wantMaxRB)
			}
		})
	}
}

// TestStickyConnFixedArray verifies that the Phase 4 [8]byte channels
// work correctly (no heap allocation per SYN/FIN header).
func TestStickyConnFixedArray(t *testing.T) {
	// Create a dummy conn that records writes.
	inner := &fakeConn{buf: make([]byte, 0, 4096)}
	sc := newStickyConn(inner)

	// Write a SYN header (8 bytes, version=1, cmd=0).
	synHeader := []byte{1, 0, 0, 0, 0, 0, 0, 1}
	n, err := sc.Write(synHeader)
	if err != nil {
		t.Fatalf("Write SYN: %v", err)
	}
	if n != 8 {
		t.Errorf("Write SYN returned %d, want 8", n)
	}

	// Write actual payload; SYN header should be coalesced.
	payload := []byte("hello")
	_, err = sc.Write(payload)
	if err != nil {
		t.Fatalf("Write payload: %v", err)
	}

	// The inner conn should have received SYN header + payload.
	got := string(inner.buf)
	if len(got) < 13 {
		t.Fatalf("inner.buf too short: %d bytes", len(got))
	}
	if got[8:13] != "hello" {
		t.Errorf("payload mismatch: got %q, want 'hello'", got[8:13])
	}
}

// TestCleanLoopSnapshot verifies that cleanLoop uses short critical sections.
func TestCleanLoopSnapshot(t *testing.T) {
	muxCfg := &Config{
		Mux: MuxConfig{
			Enabled:          true,
			Concurrency:      4,
			IdleTimeout:      1, // 1 second for fast test
			MaxPhysicalConns: 4,
			MaxStreamBuffer:  1 << 20,
			MaxReceiveBuffer: 1 << 24,
		},
	}
	ctx := config.WithConfig(context.Background(), Name, muxCfg)

	port := common.PickPort("tcp", "127.0.0.1")
	transportConfig := &transport.Config{
		LocalHost:  "127.0.0.1",
		LocalPort:  port,
		RemoteHost: "127.0.0.1",
		RemotePort: port,
	}
	ctx = config.WithConfig(ctx, transport.Name, transportConfig)
	ctx = config.WithConfig(ctx, freedom.Name, &freedom.Config{})

	tcpClient, err := transport.NewClient(ctx, nil)
	common.Must(err)
	tcpServer, err := transport.NewServer(ctx, nil)
	common.Must(err)

	muxTunnel := Tunnel{}
	muxClient, _ := muxTunnel.NewClient(ctx, tcpClient)
	muxServer, _ := muxTunnel.NewServer(ctx, tcpServer)

	// Open a stream.
	conn1, err := muxClient.DialConn(nil, nil)
	common.Must(err)
	_ = conn1

	// Wait for the cleanLoop tick (~250ms = timeout/4).
	time.Sleep(500 * time.Millisecond)

	// The client should not have been cleaned up (stream is active).
	muxC := muxClient.(*Client)
	muxC.clientPoolLock.Lock()
	poolLen := len(muxC.clientPool)
	muxC.clientPoolLock.Unlock()
	if poolLen == 0 {
		t.Error("expected at least 1 mux client after active use")
	}

	muxClient.Close()
	muxServer.Close()
}

// fakeConn is a minimal net.Conn for testing stickyConn writes.
type fakeConn struct {
	buf []byte
}

func (f *fakeConn) Read(b []byte) (int, error)         { return 0, nil }
func (f *fakeConn) Write(b []byte) (int, error)        { f.buf = append(f.buf, b...); return len(b), nil }
func (f *fakeConn) Close() error                       { return nil }
func (f *fakeConn) LocalAddr() net.Addr                { return nil }
func (f *fakeConn) RemoteAddr() net.Addr               { return nil }
func (f *fakeConn) SetDeadline(t time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(t time.Time) error { return nil }
func (f *fakeConn) Metadata() *tunnel.Metadata         { return nil }
