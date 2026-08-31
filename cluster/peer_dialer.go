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

	"github.com/xtaci/smux"
	"golang.org/x/net/websocket"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

const (
	// defaultPeerDialTimeout bounds the total time a single peer tunnel
	// setup may take (TCP + TLS + WS + trojan header). Every phase is
	// covered by this deadline so a peer that accepts TCP but never
	// completes the handshake cannot hang the prober or a relay forever.
	defaultPeerDialTimeout = 10 * time.Second

	// muxIdleTimeout is how long a mux session with zero active streams
	// is kept before being torn down.
	muxIdleTimeout = 60 * time.Second

	// muxDefaultConcurrency is the default max number of streams shared
	// over one peer mux session.
	muxDefaultConcurrency = 8

	// muxMagicDomain is the well-known trojan-go mux session address.
	muxMagicDomain = "MUX_CONN"
)

// trojan protocol commands used in headers.
const (
	cmdConnect byte = 0x01
	cmdMux     byte = 0x7f
)

// PeerDialer encapsulates a connection to a peer node using the full
// tunnel stack: TCP → TLS → WebSocket → Trojan protocol.
// Each peer has its own dialer instance with pre-computed auth hash.
//
// When mux is enabled for the peer, relay connections reuse a shared smux
// session (one authenticated trojan tunnel carrying many streams) instead
// of paying the full handshake per relay; probes still work unchanged
// because each stream is an independent end-to-end connection.
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

	dialTimeout time.Duration

	muxEnable      bool
	muxConcurrency int

	ctx    context.Context
	cancel context.CancelFunc

	muxMu       sync.Mutex
	muxSessions []*peerMuxSession
}

// peerMuxSession is one smux session over a full trojan tunnel to the peer.
type peerMuxSession struct {
	session    *smux.Session
	conn       net.Conn
	lastActive time.Time
}

func NewPeerDialer(ctx context.Context, peer PeerConfig) (*PeerDialer, error) {
	ctx, cancel := context.WithCancel(ctx)

	sni := peer.SSL.SNI
	if sni == "" {
		sni = peer.Host
	}

	// Certificate verification is ON unless the operator explicitly sets
	// "verify": false (issue #12). The old bool zero-value silently skipped
	// verification for every config that did not copy the README example.
	verify := true
	if peer.SSL.Verify != nil {
		verify = *peer.SSL.Verify
	}
	if !verify {
		log.Warnf("cluster: peer %s has TLS certificate verification DISABLED; relay traffic is exposed to MITM", peer.Name)
	}

	concurrency := peer.Mux.Concurrency
	if concurrency <= 0 {
		concurrency = muxDefaultConcurrency
	}

	pd := &PeerDialer{
		name:           peer.Name,
		host:           peer.Host,
		port:           peer.Port,
		sni:            sni,
		verify:         verify,
		wsHost:         peer.Websocket.Host,
		wsPath:         peer.Websocket.Path,
		wsEnable:       peer.Websocket.Enabled,
		passHash:       hexSHA224(peer.Password),
		dialTimeout:    defaultPeerDialTimeout,
		muxEnable:      peer.Mux.Enabled,
		muxConcurrency: concurrency,
		ctx:            ctx,
		cancel:         cancel,
	}

	if pd.wsEnable && pd.wsPath == "" {
		pd.wsPath = "/"
	}
	if pd.wsEnable && pd.wsHost == "" {
		pd.wsHost = peer.Host
	}

	log.Infof("cluster: peer dialer created: %s (%s:%d ws=%v mux=%v)",
		peer.Name, peer.Host, peer.Port, pd.wsEnable, pd.muxEnable)
	return pd, nil
}

// DialConn establishes a connection to the target through the peer,
// bounded by the dialer's default timeout.
func (pd *PeerDialer) DialConn(addr *tunnel.Address) (net.Conn, error) {
	return pd.DialConnWithTimeout(addr, pd.dialTimeout)
}

// DialConnWithTimeout establishes a connection to the target through the
// peer's trojan server. Every handshake phase is bounded by timeout so a
// half-open peer can never wedge the probe loop or a relay goroutine.
//
// With mux enabled, a stream is opened on a shared session; on any mux
// error it falls back to a dedicated full-stack connection.
func (pd *PeerDialer) DialConnWithTimeout(addr *tunnel.Address, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = pd.dialTimeout
	}
	if pd.muxEnable {
		conn, err := pd.dialMuxStream(addr, timeout)
		if err == nil {
			return conn, nil
		}
		log.DebugKV("cluster: peer mux stream failed, falling back to dedicated tunnel",
			"peer", pd.name, "target", addr.String(), "err", err)
	}
	return pd.dialDedicated(addr, timeout)
}

// dialDedicated builds one full tunnel connection for a single relay:
// TCP → TLS → (WS) → trojan header with the target address.
func (pd *PeerDialer) dialDedicated(addr *tunnel.Address, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(pd.ctx, timeout)
	defer cancel()

	protoConn, err := pd.dialTransport(ctx, time.Now().Add(timeout))
	if err != nil {
		return nil, err
	}

	// Step 4: Send Trojan protocol header
	if err := pd.writeTrojanHeader(protoConn, addr); err != nil {
		protoConn.Close()
		return nil, fmt.Errorf("peer %s: trojan header: %w", pd.name, err)
	}

	// Handshake done — clear the deadline for the data phase.
	protoConn.SetDeadline(time.Time{})
	return protoConn, nil
}

// dialTransport establishes the transport layers to the peer:
// TCP → TLS → optional WebSocket upgrade. The context bounds the TCP dial
// and the TLS handshake; the conn deadline additionally bounds the WS
// upgrade, which the websocket helper does not make cancellable.
func (pd *PeerDialer) dialTransport(ctx context.Context, deadline time.Time) (net.Conn, error) {
	dialAddr := fmt.Sprintf("%s:%d", pd.host, pd.port)

	// Step 1: TCP connect to peer
	var d net.Dialer
	tcpConn, err := d.DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		return nil, fmt.Errorf("peer %s: tcp dial: %w", pd.name, err)
	}
	// One deadline covering every remaining handshake write/read.
	tcpConn.SetDeadline(deadline)

	// Step 2: TLS handshake
	tlsConf := &tls.Config{
		ServerName:         pd.sni,
		InsecureSkipVerify: !pd.verify,
	}
	tlsConn := tls.Client(tcpConn, tlsConf)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
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

	return protoConn, nil
}

// dialMuxStream opens a stream on a shared smux session to the peer and
// writes the per-stream (simplesocks-style) header carrying the target.
func (pd *PeerDialer) dialMuxStream(addr *tunnel.Address, timeout time.Duration) (net.Conn, error) {
	stream, err := pd.openMuxStream(timeout)
	if err != nil {
		return nil, err
	}

	// Per-stream header: CMD(1) + address, as consumed by the peer's
	// simplesocks layer above its mux server.
	md := &tunnel.Metadata{Command: tunnel.Command(cmdConnect), Address: addr}
	if err := md.WriteTo(stream); err != nil {
		stream.Close()
		return nil, fmt.Errorf("peer %s: mux stream header: %w", pd.name, err)
	}
	return stream, nil
}

// openMuxStream returns a fresh stream on a session below the concurrency
// cap, creating a new session when needed. Closed and idle sessions are
// reaped on the way.
func (pd *PeerDialer) openMuxStream(timeout time.Duration) (*smux.Stream, error) {
	pd.muxMu.Lock()
	defer pd.muxMu.Unlock()

	now := time.Now()

	// Reap closed sessions and sessions idle with no streams.
	kept := pd.muxSessions[:0]
	for _, s := range pd.muxSessions {
		if s.session.IsClosed() || (s.session.NumStreams() == 0 && now.Sub(s.lastActive) > muxIdleTimeout) {
			s.session.Close()
			s.conn.Close()
			continue
		}
		kept = append(kept, s)
	}
	pd.muxSessions = kept

	// Reuse an existing session with capacity.
	for _, s := range pd.muxSessions {
		if s.session.NumStreams() < pd.muxConcurrency {
			stream, err := s.session.OpenStream()
			if err != nil {
				// Session went bad mid-flight; drop it and try the next.
				s.session.Close()
				s.conn.Close()
				continue
			}
			s.lastActive = now
			return stream, nil
		}
	}

	// All sessions at capacity — establish a new one.
	sess, conn, err := pd.newMuxSession(timeout)
	if err != nil {
		return nil, err
	}
	info := &peerMuxSession{session: sess, conn: conn, lastActive: now}
	stream, err := sess.OpenStream()
	if err != nil {
		sess.Close()
		conn.Close()
		return nil, fmt.Errorf("peer %s: open first mux stream: %w", pd.name, err)
	}
	pd.muxSessions = append(pd.muxSessions, info)
	return stream, nil
}

// newMuxSession builds one full tunnel to the peer and upgrades it to an
// smux session using the trojan mux command.
func (pd *PeerDialer) newMuxSession(timeout time.Duration) (*smux.Session, net.Conn, error) {
	ctx, cancel := context.WithTimeout(pd.ctx, timeout)
	defer cancel()

	transport, err := pd.dialTransport(ctx, time.Now().Add(timeout))
	if err != nil {
		return nil, nil, err
	}

	// Trojan header for the session conn: CMD=Mux toward the magic domain.
	muxAddr := tunnel.NewAddressFromHostPort("tcp", muxMagicDomain, 0)
	if err := pd.writeTrojanHeaderCmd(transport, muxAddr, cmdMux); err != nil {
		transport.Close()
		return nil, nil, fmt.Errorf("peer %s: mux session header: %w", pd.name, err)
	}

	smuxConfig := smux.DefaultConfig()
	smuxConfig.KeepAliveInterval = 15 * time.Second
	smuxConfig.KeepAliveTimeout = 60 * time.Second
	session, err := smux.Client(transport, smuxConfig)
	if err != nil {
		transport.Close()
		return nil, nil, fmt.Errorf("peer %s: smux client: %w", pd.name, err)
	}
	return session, transport, nil
}

// Probe dials the peer to reach the target and measures RTT using the
// default dial timeout.
func (pd *PeerDialer) Probe(target ProbeTarget) time.Duration {
	return pd.ProbeWithTimeout(target, pd.dialTimeout)
}

// ProbeWithTimeout dials the peer to reach the target and measures RTT.
// After establishing the tunnel, it waits briefly to verify the peer
// can actually connect to the target (a fast EOF/RST means the peer's
// outbound dial failed). Returns the dial duration or -1 if unreachable.
func (pd *PeerDialer) ProbeWithTimeout(target ProbeTarget, timeout time.Duration) time.Duration {
	addr := tunnel.NewAddressFromHostPort("tcp", target.Host, target.Port)
	start := time.Now()
	conn, err := pd.DialConnWithTimeout(addr, timeout)
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
	tcpConn.SetDeadline(time.Now().Add(timeout))

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
	pd.muxMu.Lock()
	for _, s := range pd.muxSessions {
		s.session.Close()
		s.conn.Close()
	}
	pd.muxSessions = nil
	pd.muxMu.Unlock()
	return nil
}

func (pd *PeerDialer) Name() string {
	return pd.name
}

// writeTrojanHeader writes the trojan protocol header for a connect command:
// hex(sha224(password)) + CRLF + CMD + ATYP + DST.ADDR + DST.PORT + CRLF
func (pd *PeerDialer) writeTrojanHeader(conn net.Conn, addr *tunnel.Address) error {
	return pd.writeTrojanHeaderCmd(conn, addr, cmdConnect)
}

// writeTrojanHeaderCmd writes the trojan protocol header with the given
// command byte (cmdConnect for relays and probes, cmdMux for sessions).
func (pd *PeerDialer) writeTrojanHeaderCmd(conn net.Conn, addr *tunnel.Address, cmd byte) error {
	crlf := []byte{0x0d, 0x0a}
	buf := bytes.NewBuffer(make([]byte, 0, 512))

	// Password hash (56 hex chars)
	buf.WriteString(pd.passHash)
	buf.Write(crlf)

	// Command: Connect = 0x01, Mux = 0x7f
	buf.WriteByte(cmd)

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
