package singmux

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// --- Request Header Tests (10.2) ---

func TestReadRequest_V0Smux(t *testing.T) {
	data := []byte{0x00, 0x00} // Version0, ProtocolSmux
	req, err := ReadRequest(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if req.Version != Version0 {
		t.Errorf("version: got %d, want %d", req.Version, Version0)
	}
	if req.Protocol != ProtocolSmux {
		t.Errorf("protocol: got %d, want %d", req.Protocol, ProtocolSmux)
	}
	if req.Padding {
		t.Error("padding should be false")
	}
}

func TestReadRequest_V0Yamux(t *testing.T) {
	data := []byte{0x00, 0x01}
	req, err := ReadRequest(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if req.Protocol != ProtocolYAMux {
		t.Errorf("protocol: got %d, want %d", req.Protocol, ProtocolYAMux)
	}
}

func TestReadRequest_V1NoPadding(t *testing.T) {
	data := []byte{0x01, 0x00, 0x00} // Version1, smux, padding=false
	req, err := ReadRequest(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if req.Version != Version1 {
		t.Errorf("version: got %d, want %d", req.Version, Version1)
	}
	if req.Padding {
		t.Error("padding should be false")
	}
}

func TestReadRequest_V1WithPadding(t *testing.T) {
	padding := make([]byte, 100)
	var buf bytes.Buffer
	buf.WriteByte(0x01) // Version1
	buf.WriteByte(0x00) // ProtocolSmux
	buf.WriteByte(0x01) // padding=true
	binary.Write(&buf, binary.BigEndian, uint16(100))
	buf.Write(padding)

	req, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Padding {
		t.Error("padding should be true")
	}
	if req.Protocol != ProtocolSmux {
		t.Errorf("protocol: got %d, want %d", req.Protocol, ProtocolSmux)
	}
}

func TestReadRequest_InvalidVersion(t *testing.T) {
	data := []byte{0x02, 0x00}
	_, err := ReadRequest(bytes.NewReader(data))
	if err == nil {
		t.Error("expected error for unsupported version")
	}
}

func TestReadRequest_Empty(t *testing.T) {
	_, err := ReadRequest(bytes.NewReader(nil))
	if err == nil {
		t.Error("expected error for empty input")
	}
}

// --- StreamRequest Tests (10.2) ---

func TestReadStreamRequest_TCPIPv4(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(0x0000)) // flags: TCP
	buf.WriteByte(0x01)                                   // ATYP: IPv4
	buf.Write(net.ParseIP("1.2.3.4").To4())               // IP
	binary.Write(&buf, binary.BigEndian, uint16(443))     // port

	req, err := ReadStreamRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if req.Network != "tcp" {
		t.Errorf("network: got %s, want tcp", req.Network)
	}
	if req.Address.Port != 443 {
		t.Errorf("port: got %d, want 443", req.Address.Port)
	}
	if !req.Address.IP.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("ip: got %v, want 1.2.3.4", req.Address.IP)
	}
}

func TestReadStreamRequest_TCPDomain(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(0x0000)) // flags: TCP
	buf.WriteByte(0x03)                                   // ATYP: DomainName
	domain := "google.com"
	buf.WriteByte(byte(len(domain)))
	buf.WriteString(domain)
	binary.Write(&buf, binary.BigEndian, uint16(80))

	req, err := ReadStreamRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if req.Address.DomainName != "google.com" {
		t.Errorf("domain: got %s, want google.com", req.Address.DomainName)
	}
	if req.Address.Port != 80 {
		t.Errorf("port: got %d, want 80", req.Address.Port)
	}
}

func TestReadStreamRequest_TCPIPv6(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(0x0000))
	buf.WriteByte(0x04) // ATYP: IPv6
	ip := net.ParseIP("2001:db8::1")
	buf.Write(ip.To16())
	binary.Write(&buf, binary.BigEndian, uint16(8080))

	req, err := ReadStreamRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Address.IP.Equal(ip) {
		t.Errorf("ip: got %v, want %v", req.Address.IP, ip)
	}
	if req.Address.Port != 8080 {
		t.Errorf("port: got %d, want 8080", req.Address.Port)
	}
}

func TestReadStreamRequest_UDP(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint16(0x0001)) // flags: UDP
	buf.WriteByte(0x01)
	buf.Write(net.ParseIP("8.8.8.8").To4())
	binary.Write(&buf, binary.BigEndian, uint16(53))

	req, err := ReadStreamRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if req.Network != "udp" {
		t.Errorf("network: got %s, want udp", req.Network)
	}
}

// --- responseStream Tests (10.2) ---

type mockConn struct {
	net.Conn
	buf bytes.Buffer
	mu  sync.Mutex
}

func (m *mockConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buf.Write(p)
}

func (m *mockConn) Read(p []byte) (int, error) {
	return 0, io.EOF
}

func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(t time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func TestResponseStream_FirstWrite(t *testing.T) {
	mock := &mockConn{}
	rs := newResponseStream(mock)

	n, err := rs.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n: got %d, want 5", n)
	}

	mock.mu.Lock()
	data := mock.buf.Bytes()
	mock.mu.Unlock()

	if len(data) != 6 {
		t.Fatalf("data length: got %d, want 6", len(data))
	}
	if data[0] != statusSuccess {
		t.Errorf("first byte: got %d, want %d", data[0], statusSuccess)
	}
	if string(data[1:]) != "hello" {
		t.Errorf("payload: got %q, want %q", string(data[1:]), "hello")
	}
}

func TestResponseStream_SecondWrite(t *testing.T) {
	mock := &mockConn{}
	rs := newResponseStream(mock)

	rs.Write([]byte("hello"))

	mock.mu.Lock()
	mock.buf.Reset()
	mock.mu.Unlock()

	n, err := rs.Write([]byte("world"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("n: got %d, want 5", n)
	}

	mock.mu.Lock()
	data := mock.buf.Bytes()
	mock.mu.Unlock()

	if string(data) != "world" {
		t.Errorf("payload: got %q, want %q (no status prefix)", string(data), "world")
	}
}

func TestResponseStream_ConcurrentFirstWrite(t *testing.T) {
	mock := &mockConn{}
	rs := newResponseStream(mock)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs.Write([]byte("x"))
		}()
	}
	wg.Wait()

	mock.mu.Lock()
	data := mock.buf.Bytes()
	mock.mu.Unlock()

	statusCount := 0
	for _, b := range data {
		if b == statusSuccess {
			statusCount++
		}
	}
	if statusCount != 1 {
		t.Errorf("status byte count: got %d, want 1 (data=%x)", statusCount, data)
	}
}

// --- ProtocolName Tests ---

func TestProtocolName(t *testing.T) {
	tests := []struct {
		p    byte
		want string
	}{
		{ProtocolSmux, "smux"},
		{ProtocolYAMux, "yamux"},
		{ProtocolH2Mux, "h2mux"},
		{99, "unknown(99)"},
	}
	for _, tt := range tests {
		got := ProtocolName(tt.p)
		if got != tt.want {
			t.Errorf("ProtocolName(%d): got %q, want %q", tt.p, got, tt.want)
		}
	}
}

// --- Session Creation Tests (10.4) ---

func TestNewServerSession_H2MuxUnsupported(t *testing.T) {
	req := &Request{Version: Version0, Protocol: ProtocolH2Mux}
	_, err := newServerSession(nil, req)
	if err == nil {
		t.Error("expected error for h2mux protocol")
	}
}

func TestNewServerSession_UnknownProtocol(t *testing.T) {
	req := &Request{Version: Version0, Protocol: 99}
	_, err := newServerSession(nil, req)
	if err == nil {
		t.Error("expected error for unknown protocol")
	}
}
