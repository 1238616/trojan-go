package cluster

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

func TestHexSHA224(t *testing.T) {
	// Known test vector: SHA-224("password")
	hash := hexSHA224("password")
	if len(hash) != 56 { // SHA-224 = 28 bytes = 56 hex chars
		t.Fatalf("expected 56 hex chars, got %d: %q", len(hash), hash)
	}
	// SHA-224("password") = d63dc919e201d7bc4c825630d2cf25fdc93d4b2f0d46706d29038d01
	expected := "d63dc919e201d7bc4c825630d2cf25fdc93d4b2f0d46706d29038d01"
	if hash != expected {
		t.Fatalf("SHA-224 mismatch:\n  got:  %s\n  want: %s", hash, expected)
	}
}

func TestTrojanHeaderFormatIPv4(t *testing.T) {
	pd := &PeerDialer{
		name:     "test-peer",
		passHash: hexSHA224("test-password"),
	}

	var buf bytes.Buffer
	addr := tunnel.NewAddressFromHostPort("tcp", "149.154.175.53", 443)
	err := pd.writeTrojanHeader(&fakeWriteConn{buf: &buf}, addr)
	if err != nil {
		t.Fatalf("writeTrojanHeader failed: %v", err)
	}

	data := buf.Bytes()

	// Check password hash (56 bytes)
	if len(data) < 56 {
		t.Fatalf("data too short: %d bytes", len(data))
	}
	passHash := string(data[:56])
	if passHash != pd.passHash {
		t.Fatalf("password hash mismatch")
	}

	// CRLF after hash
	if data[56] != 0x0d || data[57] != 0x0a {
		t.Fatal("missing CRLF after password hash")
	}

	// Command: Connect = 0x01
	if data[58] != 0x01 {
		t.Fatalf("expected CMD=0x01, got 0x%02x", data[58])
	}

	// ATYP: IPv4 = 0x01
	if data[59] != 0x01 {
		t.Fatalf("expected ATYP=0x01 (IPv4), got 0x%02x", data[59])
	}

	// IP: 149.154.175.53
	ip := net.IP(data[60:64])
	expectedIP := net.ParseIP("149.154.175.53").To4()
	if !ip.Equal(expectedIP) {
		t.Fatalf("expected IP %v, got %v", expectedIP, ip)
	}

	// Port: 443 = 0x01BB (big-endian)
	port := int(data[64])<<8 | int(data[65])
	if port != 443 {
		t.Fatalf("expected port 443, got %d", port)
	}

	// Trailing CRLF
	if data[66] != 0x0d || data[67] != 0x0a {
		t.Fatal("missing trailing CRLF")
	}
}

func TestTrojanHeaderFormatDomain(t *testing.T) {
	pd := &PeerDialer{
		passHash: hexSHA224("secret"),
	}

	var buf bytes.Buffer
	addr := &tunnel.Address{
		DomainName:  "telegram.org",
		Port:        443,
		AddressType: tunnel.DomainName,
	}
	err := pd.writeTrojanHeader(&fakeWriteConn{buf: &buf}, addr)
	if err != nil {
		t.Fatalf("writeTrojanHeader failed: %v", err)
	}

	data := buf.Bytes()

	// Skip hash(56) + CRLF(2) + CMD(1) = 59
	if data[59] != 0x03 {
		t.Fatalf("expected ATYP=0x03 (Domain), got 0x%02x", data[59])
	}

	// Domain length
	domainLen := int(data[60])
	if domainLen != len("telegram.org") {
		t.Fatalf("expected domain len=%d, got %d", len("telegram.org"), domainLen)
	}

	// Domain value
	domain := string(data[61 : 61+domainLen])
	if domain != "telegram.org" {
		t.Fatalf("expected domain=telegram.org, got %q", domain)
	}
}

func TestPeerDialerCreateWithWebsocket(t *testing.T) {
	ctx := context.Background()
	peer := PeerConfig{
		Name:     "tokyo-1",
		Host:     "tokyo.example.com",
		Port:     443,
		Password: "secret",
		Websocket: PeerWebsocketConfig{
			Enabled: true,
			Host:    "ws.tokyo.example.com",
			Path:    "/tunnel",
		},
		SSL: PeerSSLConfig{
			SNI:    "tokyo.example.com",
			Verify: true,
		},
	}

	pd, err := NewPeerDialer(ctx, peer)
	if err != nil {
		t.Fatalf("NewPeerDialer failed: %v", err)
	}
	defer pd.Close()

	if pd.name != "tokyo-1" {
		t.Fatalf("expected name=tokyo-1, got %q", pd.name)
	}
	if !pd.wsEnable {
		t.Fatal("expected wsEnable=true")
	}
	if pd.wsHost != "ws.tokyo.example.com" {
		t.Fatalf("expected wsHost=ws.tokyo.example.com, got %q", pd.wsHost)
	}
	if pd.wsPath != "/tunnel" {
		t.Fatalf("expected wsPath=/tunnel, got %q", pd.wsPath)
	}
	if pd.sni != "tokyo.example.com" {
		t.Fatalf("expected sni=tokyo.example.com, got %q", pd.sni)
	}
	if !pd.verify {
		t.Fatal("expected verify=true")
	}
}

func TestPeerDialerCreateWithoutWebsocket(t *testing.T) {
	ctx := context.Background()
	peer := PeerConfig{
		Name:     "la-1",
		Host:     "la.example.com",
		Port:     443,
		Password: "secret",
	}

	pd, err := NewPeerDialer(ctx, peer)
	if err != nil {
		t.Fatalf("NewPeerDialer failed: %v", err)
	}
	defer pd.Close()

	if pd.wsEnable {
		t.Fatal("expected wsEnable=false")
	}
	if pd.sni != "la.example.com" {
		t.Fatalf("expected sni default to host, got %q", pd.sni)
	}
}

func TestPeerDialerDefaultWSPath(t *testing.T) {
	ctx := context.Background()
	peer := PeerConfig{
		Name:     "test",
		Host:     "example.com",
		Port:     443,
		Password: "pw",
		Websocket: PeerWebsocketConfig{
			Enabled: true,
			// Path and Host empty → should default
		},
	}

	pd, err := NewPeerDialer(ctx, peer)
	if err != nil {
		t.Fatalf("NewPeerDialer failed: %v", err)
	}
	defer pd.Close()

	if pd.wsPath != "/" {
		t.Fatalf("expected default wsPath=/, got %q", pd.wsPath)
	}
	if pd.wsHost != "example.com" {
		t.Fatalf("expected default wsHost=example.com, got %q", pd.wsHost)
	}
}

// TestTrojanHeaderMuxSessionFormat verifies the session-establishment header
// uses CMD=Mux (0x7f) toward the well-known MUX_CONN domain, matching what
// the peer's trojan server routes into its mux channel.
func TestTrojanHeaderMuxSessionFormat(t *testing.T) {
	pd := &PeerDialer{
		name:     "test-peer",
		passHash: hexSHA224("test-password"),
	}

	var buf bytes.Buffer
	addr := tunnel.NewAddressFromHostPort("tcp", muxMagicDomain, 0)
	if err := pd.writeTrojanHeaderCmd(&fakeWriteConn{buf: &buf}, addr, cmdMux); err != nil {
		t.Fatalf("writeTrojanHeaderCmd failed: %v", err)
	}
	data := buf.Bytes()

	if string(data[:56]) != pd.passHash {
		t.Fatal("password hash mismatch")
	}
	if data[56] != 0x0d || data[57] != 0x0a {
		t.Fatal("missing CRLF after hash")
	}
	if data[58] != cmdMux {
		t.Fatalf("expected CMD=0x7f (Mux), got 0x%02x", data[58])
	}
	if data[59] != 0x03 {
		t.Fatalf("expected ATYP=0x03 (domain), got 0x%02x", data[59])
	}
	domainLen := int(data[60])
	if got := string(data[61 : 61+domainLen]); got != muxMagicDomain {
		t.Fatalf("expected domain=%s, got %q", muxMagicDomain, got)
	}
	// Trailing CRLF after the port.
	last := data[len(data)-2:]
	if last[0] != 0x0d || last[1] != 0x0a {
		t.Fatal("missing trailing CRLF")
	}
}

// TestPeerDialerMuxFallback verifies that when mux is enabled but the peer
// is unreachable, the mux attempt degrades to the dedicated dial path and
// fails fast instead of hanging.
func TestPeerDialerMuxFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pd, err := NewPeerDialer(ctx, PeerConfig{
		Name: "unreachable", Host: "127.0.0.1", Port: 19999, Password: "x",
		Mux:  PeerMuxConfig{Enabled: true, Concurrency: 4},
	})
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	defer pd.Close()

	if !pd.muxEnable || pd.muxConcurrency != 4 {
		t.Fatalf("mux config not applied: enable=%v concurrency=%d", pd.muxEnable, pd.muxConcurrency)
	}

	addr := tunnel.NewAddressFromHostPort("tcp", "1.2.3.4", 443)
	start := time.Now()
	if _, err := pd.DialConnWithTimeout(addr, 2*time.Second); err == nil {
		t.Fatal("expected dial failure against unreachable peer")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("mux fallback path too slow: %v", elapsed)
	}
}

// fakeWriteConn captures writes for header format verification.
type fakeWriteConn struct {
	buf *bytes.Buffer
}

func (f *fakeWriteConn) Write(p []byte) (int, error)         { return f.buf.Write(p) }
func (f *fakeWriteConn) Read(p []byte) (int, error)          { return 0, nil }
func (f *fakeWriteConn) Close() error                        { return nil }
func (f *fakeWriteConn) LocalAddr() net.Addr                 { return nil }
func (f *fakeWriteConn) RemoteAddr() net.Addr                { return nil }
func (f *fakeWriteConn) SetDeadline(_ time.Time) error       { return nil }
func (f *fakeWriteConn) SetReadDeadline(_ time.Time) error   { return nil }
func (f *fakeWriteConn) SetWriteDeadline(_ time.Time) error  { return nil }
