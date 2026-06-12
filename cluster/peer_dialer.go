package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// PeerDialer encapsulates a connection to a peer node using the full
// tunnel stack: TCP → TLS → WebSocket → Trojan protocol.
// Each peer has its own dialer instance with pre-computed auth hash.
type PeerDialer struct {
	name     string
	host     string
	port     int
	sni      string
	verify   bool
	wsHost   string
	wsPath   string
	wsEnable bool
	passHash string // hex(sha224(password))

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

func NewPeerDialer(ctx context.Context, peer PeerConfig) (*PeerDialer, error) {
	ctx, cancel := context.WithCancel(ctx)

	sni := peer.SSL.SNI
	if sni == "" {
		sni = peer.Host
	}

	pd := &PeerDialer{
		name:     peer.Name,
		host:     peer.Host,
		port:     peer.Port,
		sni:      sni,
		verify:   peer.SSL.Verify,
		wsHost:   peer.Websocket.Host,
		wsPath:   peer.Websocket.Path,
		wsEnable: peer.Websocket.Enabled,
		passHash: hexSHA224(peer.Password),
		ctx:      ctx,
		cancel:   cancel,
	}

	if pd.wsEnable && pd.wsPath == "" {
		pd.wsPath = "/"
	}
	if pd.wsEnable && pd.wsHost == "" {
		pd.wsHost = peer.Host
	}

	log.Infof("cluster: peer dialer created: %s (%s:%d ws=%v)",
		peer.Name, peer.Host, peer.Port, pd.wsEnable)
	return pd, nil
}

// DialConn establishes a connection to the target through the peer's
// trojan server, using the full stack: TCP → TLS → WebSocket → Trojan.
func (pd *PeerDialer) DialConn(addr *tunnel.Address) (net.Conn, error) {
	// Step 1: TCP connect to peer
	dialAddr := fmt.Sprintf("%s:%d", pd.host, pd.port)
	tcpConn, err := net.DialTimeout("tcp", dialAddr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("peer %s: tcp dial: %w", pd.name, err)
	}

	// Step 2: TLS handshake
	tlsConf := &tls.Config{
		ServerName:         pd.sni,
		InsecureSkipVerify: !pd.verify,
	}
	tlsConn := tls.Client(tcpConn, tlsConf)
	if err := tlsConn.HandshakeContext(pd.ctx); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("peer %s: tls handshake: %w", pd.name, err)
	}

	var protoConn net.Conn = tlsConn

	// Step 3: WebSocket upgrade (if enabled)
	if pd.wsEnable {
		wsURL := "wss://" + pd.wsHost + pd.wsPath
		origin := "https://" + pd.wsHost
		wsConfig, err := websocket.NewConfig(wsURL, origin)
		if err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("peer %s: ws config: %w", pd.name, err)
		}
		wsConn, err := websocket.NewClient(wsConfig, tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, fmt.Errorf("peer %s: ws handshake: %w", pd.name, err)
		}
		protoConn = wsConn
	}

	// Step 4: Send Trojan protocol header
	if err := pd.writeTrojanHeader(protoConn, addr); err != nil {
		protoConn.Close()
		return nil, fmt.Errorf("peer %s: trojan header: %w", pd.name, err)
	}

	return protoConn, nil
}

// Probe dials the peer to reach the target and measures RTT.
// After establishing the tunnel, it waits briefly to verify the peer
// can actually connect to the target (a fast EOF/RST means the peer's
// outbound dial failed). Returns the dial duration or -1 if unreachable.
func (pd *PeerDialer) Probe(target ProbeTarget) time.Duration {
	addr := tunnel.NewAddressFromHostPort("tcp", target.Host, target.Port)
	start := time.Now()
	conn, err := pd.DialConn(addr)
	if err != nil {
		log.DebugKV("cluster: probe via peer failed",
			"peer", pd.name, "target", fmt.Sprintf("%s:%d", target.Host, target.Port),
			"err", err)
		return -1
	}
	rtt := time.Since(start)

	// Verify end-to-end: if the peer cannot reach the target, it will
	// close the connection quickly (RST or FIN). Wait up to 2s for
	// such a signal. If the connection stays alive, the target is
	// reachable through this peer.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, readErr := conn.Read(buf)
	conn.Close()

	if readErr != nil {
		// Timeout means the connection is still alive (peer connected
		// to target and is waiting for data) — this is the success case.
		if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
			return rtt
		}
		// EOF or connection reset means peer couldn't reach the target.
		log.DebugKV("cluster: probe peer connected but target unreachable",
			"peer", pd.name, "target", fmt.Sprintf("%s:%d", target.Host, target.Port),
			"read_err", readErr)
		return -1
	}

	// Got data back (unlikely for a probe), still counts as reachable.
	return rtt
}

// CheckAlive verifies connectivity to the peer (TCP+TLS+WS handshake only,
// no Trojan header or target required). Returns RTT or -1 on failure.
func (pd *PeerDialer) CheckAlive(timeout time.Duration) time.Duration {
	dialAddr := fmt.Sprintf("%s:%d", pd.host, pd.port)
	start := time.Now()

	tcpConn, err := net.DialTimeout("tcp", dialAddr, timeout)
	if err != nil {
		return -1
	}

	tlsConf := &tls.Config{
		ServerName:         pd.sni,
		InsecureSkipVerify: !pd.verify,
	}
	tlsConn := tls.Client(tcpConn, tlsConf)
	ctx, cancel := context.WithTimeout(pd.ctx, timeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tcpConn.Close()
		return -1
	}

	if pd.wsEnable {
		wsURL := "wss://" + pd.wsHost + pd.wsPath
		origin := "https://" + pd.wsHost
		wsConfig, cfgErr := websocket.NewConfig(wsURL, origin)
		if cfgErr != nil {
			tlsConn.Close()
			return -1
		}
		wsConn, wsErr := websocket.NewClient(wsConfig, tlsConn)
		if wsErr != nil {
			tlsConn.Close()
			return -1
		}
		wsConn.Close()
	} else {
		tlsConn.Close()
	}

	return time.Since(start)
}

func (pd *PeerDialer) Close() error {
	pd.cancel()
	return nil
}

func (pd *PeerDialer) Name() string {
	return pd.name
}

// writeTrojanHeader writes the trojan protocol header:
// hex(sha224(password)) + CRLF + CMD + ATYP + DST.ADDR + DST.PORT + CRLF
func (pd *PeerDialer) writeTrojanHeader(conn net.Conn, addr *tunnel.Address) error {
	crlf := []byte{0x0d, 0x0a}
	buf := bytes.NewBuffer(make([]byte, 0, 512))

	// Password hash (56 hex chars)
	buf.WriteString(pd.passHash)
	buf.Write(crlf)

	// Command: Connect = 0x01
	buf.WriteByte(0x01)

	// Address
	switch addr.AddressType {
	case tunnel.IPv4:
		buf.WriteByte(0x01) // ATYP IPv4
		ip4 := addr.IP.To4()
		buf.Write(ip4)
	case tunnel.IPv6:
		buf.WriteByte(0x04) // ATYP IPv6
		buf.Write(addr.IP.To16())
	case tunnel.DomainName:
		buf.WriteByte(0x03) // ATYP Domain
		domain := []byte(addr.DomainName)
		buf.WriteByte(byte(len(domain)))
		buf.Write(domain)
	}

	// Port (big-endian)
	buf.WriteByte(byte(addr.Port >> 8))
	buf.WriteByte(byte(addr.Port & 0xff))

	buf.Write(crlf)

	_, err := conn.Write(buf.Bytes())
	return err
}

// hexSHA224 computes SHA-224 of password and returns hex string.
// Trojan protocol uses SHA-224 for password hashing.
func hexSHA224(password string) string {
	hash := sha256.New224()
	hash.Write([]byte(password))
	return hex.EncodeToString(hash.Sum(nil))
}
