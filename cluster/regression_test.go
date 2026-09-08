package cluster

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"

	"github.com/p4gefau1t/trojan-go/test/util"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// --- Regression for bug #1: route selection was non-deterministic ---
// match() ignored the port and returned whichever entry a randomized Go map
// iteration hit first. Same host on two ports flipped between peers.

func TestRouteTableMatchDeterministicPerPort(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Same host, two ports, two different best peers.
	rt.Update("1.2.3.4:443", "local", 200*time.Millisecond)
	rt.Update("1.2.3.4:443", "peer-a", 10*time.Millisecond)
	rt.Update("1.2.3.4:8080", "local", 200*time.Millisecond)
	rt.Update("1.2.3.4:8080", "peer-b", 10*time.Millisecond)

	for i := 0; i < 200; i++ {
		peer, _ := rt.BestExit("1.2.3.4", 443)
		if peer != "peer-a" {
			t.Fatalf("iter %d: :443 expected peer-a, got %q", i, peer)
		}
		peer, _ = rt.BestExit("1.2.3.4", 8080)
		if peer != "peer-b" {
			t.Fatalf("iter %d: :8080 expected peer-b, got %q", i, peer)
		}
	}
}

func TestRouteTableMatchHostFallbackDeterministic(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Only one port recorded; a query for a different port must fall back to
	// the host entry deterministically.
	rt.Update("9.9.9.9:443", "local", 300*time.Millisecond)
	rt.Update("9.9.9.9:443", "peer-x", 15*time.Millisecond)

	for i := 0; i < 200; i++ {
		peer, gain := rt.BestExit("9.9.9.9", 12345)
		if peer != "peer-x" {
			t.Fatalf("iter %d: host fallback expected peer-x, got %q", i, peer)
		}
		if gain != 285*time.Millisecond {
			t.Fatalf("iter %d: expected gain 285ms, got %v", i, gain)
		}
	}
}

// --- Regression for bug #2: Snapshot() mislabelled TargetRTT ---
// Two independent map iterations zipped target names and RTT values from
// different orderings, so the per-target latency shown by /api/cluster was
// scrambled. Verify the label always matches the value.

func buildSnapshotRouter(t *testing.T) *ClusterRouter {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &Config{
		Enabled:          true,
		NodeName:         "sg-1",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		RelayThreshold:   50,
		LatencyThreshold: 100,
		Peers: []PeerConfig{
			{Name: "peer-x", Host: "peer.example.com", Port: 443, Password: "s"},
		},
	}

	rt := NewRouteTable("sg-1", 50)
	// Distinct RTT per target so any misalignment is detectable.
	rttByTarget := map[string]time.Duration{
		"1.1.1.1:443": 10 * time.Millisecond,
		"2.2.2.2:443": 20 * time.Millisecond,
		"3.3.3.3:443": 30 * time.Millisecond,
		"4.4.4.4:443": 40 * time.Millisecond,
	}
	for target, rtt := range rttByTarget {
		rt.Update(target, "sg-1", 200*time.Millisecond)
		rt.Update(target, "peer-x", rtt)
	}

	metrics := NewClusterMetrics()
	peerDialers := make(map[string]*PeerDialer)
	pd, err := NewPeerDialer(ctx, cfg.Peers[0])
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	peerDialers["peer-x"] = pd
	prober := NewProber(ctx, cfg, rt, peerDialers, metrics)

	return &ClusterRouter{
		enabled:     true,
		routeTable:  rt,
		prober:      prober,
		peerDialers: peerDialers,
		matcher:     NewTargetMatcher(nil),
		metrics:     metrics,
		localName:   "sg-1",
		cfg:         cfg,
	}
}

func TestRouterSnapshotTargetRTTLabelsMatch(t *testing.T) {
	cr := buildSnapshotRouter(t)

	expected := map[string]float64{
		"1.1.1.1:443": 10,
		"2.2.2.2:443": 20,
		"3.3.3.3:443": 30,
		"4.4.4.4:443": 40,
	}

	// Run repeatedly: the old bug depended on map iteration order, so a
	// single pass could get lucky.
	for i := 0; i < 50; i++ {
		snap := cr.Snapshot()
		if len(snap.Peers) != 1 {
			t.Fatalf("iter %d: expected 1 peer, got %d", i, len(snap.Peers))
		}
		ps := snap.Peers[0]
		if len(ps.TargetRTTs) != len(expected) {
			t.Fatalf("iter %d: expected %d TargetRTTs, got %d", i, len(expected), len(ps.TargetRTTs))
		}
		for _, trtt := range ps.TargetRTTs {
			want, ok := expected[trtt.Target]
			if !ok {
				t.Fatalf("iter %d: unexpected target %q", i, trtt.Target)
			}
			if trtt.RTTMs != want {
				t.Fatalf("iter %d: target %q labelled RTT %.1f, want %.1f (misaligned)",
					i, trtt.Target, trtt.RTTMs, want)
			}
		}
	}
}

// --- Regression for bug #3: DynamicTarget data race ---
// RegisterSlowTarget mutated shared fields in place while probe/evict/
// snapshot goroutines read them. Hammer it concurrently; `go test -race`
// flags the old in-place mutation.

func TestProberRegisterSlowTargetConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 10, // low threshold so every RTT registers
	}
	rt := NewRouteTable("local", 50)
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), NewClusterMetrics())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: register/refresh the same target from many goroutines.
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rtt := time.Duration(100+id) * time.Millisecond
				p.RegisterSlowTarget("7.7.7.7", 443, rtt, false)
			}
		}(w)
	}

	// Readers: read the stored fields and the counter concurrently.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = p.DynamicTargetCount()
				if v, ok := p.dynamicTargets.Load("7.7.7.7:443"); ok {
					dt := v.(*DynamicTarget)
					_ = dt.FirstSeenAt
					_ = dt.LastSeenAt
					_ = dt.LastDialRTT
				}
			}
		}()
	}

	// Evictor: exercises the deletion path concurrently too.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.evictStaleTargets()
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if p.DynamicTargetCount() == 0 {
		t.Fatal("expected the target to remain registered")
	}
}

// --- Regression for bug #4: peer dial had no timeout ---
// HandshakeContext used the dialer ctx (only cancelled on Close), so a peer
// that accepted TCP but never completed TLS hung the probe loop forever.

func TestPeerDialerDialConnBounded(t *testing.T) {
	host, portStr, err := net.SplitHostPort(util.BlackHoleAddr)
	if err != nil {
		t.Fatalf("bad blackhole addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd, err := NewPeerDialer(ctx, PeerConfig{
		Name: "blackhole", Host: host, Port: port, Password: "x",
	})
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	defer pd.Close()

	addr := tunnel.NewAddressFromHostPort("tcp", "10.0.0.1", 443)

	start := time.Now()
	_, err = pd.DialConnWithTimeout(addr, 400*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected dial to fail against a blackhole peer")
	}
	// Must be bounded by the timeout, not hang. Allow generous slack for CI.
	if elapsed > 3*time.Second {
		t.Fatalf("dial took %v; not bounded by 400ms timeout", elapsed)
	}
}

func TestPeerDialerProbeBounded(t *testing.T) {
	host, portStr, err := net.SplitHostPort(util.BlackHoleAddr)
	if err != nil {
		t.Fatalf("bad blackhole addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd, err := NewPeerDialer(ctx, PeerConfig{
		Name: "blackhole", Host: host, Port: port, Password: "x",
	})
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	defer pd.Close()

	start := time.Now()
	rtt := pd.ProbeWithTimeout(ProbeTarget{Host: "10.0.0.1", Port: 443}, 400*time.Millisecond)
	elapsed := time.Since(start)

	if rtt >= 0 {
		t.Fatal("expected probe to report unreachable (-1)")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe took %v; not bounded", elapsed)
	}
}

// --- Regression for bug #5: slow-but-successful dial mis-flagged unreachable ---
// Only an actual failed dial may mark local unreachable. A slow successful
// dial must never flip local to unavailable.

func TestProberSlowSuccessDoesNotMarkUnreachable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 100,
	}
	rt := NewRouteTable("local", 50)
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), NewClusterMetrics())

	key := "5.5.5.5:443"
	rt.Update(key, "local", 50*time.Millisecond) // local reachable

	// Slow (6s, well past the old hardcoded 5s) but SUCCESSFUL dial.
	p.RegisterSlowTarget("5.5.5.5", 443, 6*time.Second, false)

	if entryUnavailable(rt, key, "local") {
		t.Fatal("slow-but-successful dial wrongly marked local unreachable")
	}

	// Now a genuinely failed dial must mark local unreachable.
	p.RegisterSlowTarget("5.5.5.5", 443, 6*time.Second, true)
	if !entryUnavailable(rt, key, "local") {
		t.Fatal("failed dial did not mark local unreachable")
	}
}

func entryUnavailable(rt *RouteTable, key, peer string) bool {
	for _, pl := range rt.GetAllEntries()[key] {
		if pl.PeerName == peer {
			return !pl.Available
		}
	}
	return false
}

// --- Regression for P1 #8: DialAnyPeer negative cache ---
// A target that fails through every peer is cached briefly so a hot dead
// target doesn't fan out a full peer handshake per user connection.

func TestRouterDialAnyPeerNegativeCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:          true,
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		RelayThreshold:   50,
		LatencyThreshold: 100,
		Peers: []PeerConfig{
			{Name: "p-alive-but-broken", Host: "127.0.0.1", Port: 19999, Password: "x"},
		},
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	dialers := make(map[string]*PeerDialer)
	pd, _ := NewPeerDialer(ctx, cfg.Peers[0])
	dialers["p-alive-but-broken"] = pd
	prober := NewProber(ctx, cfg, rt, dialers, metrics)
	prober.peerAlive.Store("p-alive-but-broken", peerAliveInfo{Alive: true})

	cr := &ClusterRouter{
		enabled:     true,
		routeTable:  rt,
		prober:      prober,
		peerDialers: dialers,
		matcher:     NewTargetMatcher(nil),
		metrics:     metrics,
		localName:   "local",
		cfg:         cfg,
	}

	addr := &tunnel.Address{
		IP:          net.ParseIP("1.2.3.4"),
		Port:        80,
		AddressType: tunnel.IPv4,
	}

	// First attempt dials the peer and fails, populating the negative cache.
	if _, _, err := cr.DialAnyPeer(addr); err == nil {
		t.Fatal("expected first DialAnyPeer to fail")
	}

	// Second attempt for the same target must short-circuit via the cache.
	start := time.Now()
	_, _, err := cr.DialAnyPeer(addr)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected cached failure")
	}
	if !strings.Contains(err.Error(), "suppressed") {
		t.Fatalf("expected suppression error, got %q", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("negative cache did not short-circuit (took %v)", elapsed)
	}
}

// --- Regression for issue #20: IPv6 dial addresses ---
// fmt.Sprintf("%s:%d") produced "2001:db8::1:443" for IPv6 hosts, which
// net.Dial rejects. All cluster dial paths must use net.JoinHostPort.

func TestPeerDialAddrFormatsIPv6(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"1.2.3.4", 443, "1.2.3.4:443"},
		{"2001:db8::1", 443, "[2001:db8::1]:443"},
		{"::1", 80, "[::1]:80"},
		{"example.com", 443, "example.com:443"},
	}
	for _, c := range cases {
		got := peerDialAddr(c.host, c.port)
		if got != c.want {
			t.Fatalf("peerDialAddr(%q, %d) = %q, want %q", c.host, c.port, got, c.want)
		}
		// The formatted address must be acceptable to net.SplitHostPort,
		// which is what the dialers effectively require.
		if _, _, err := net.SplitHostPort(got); err != nil {
			t.Fatalf("peerDialAddr(%q, %d) produced undialable %q: %v", c.host, c.port, got, err)
		}
	}
}

func TestProbeDirectIPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("bad listener addr %q: %v", ln.Addr(), err)
	}
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewProber(ctx, &Config{NodeName: "local", ProbeTimeout: 3000, LatencyThreshold: 100},
		NewRouteTable("local", 50), make(map[string]*PeerDialer), NewClusterMetrics())

	rtt := p.probeDirect(ProbeTarget{Host: "::1", Port: port})
	if rtt < 0 {
		t.Fatalf("probeDirect against IPv6 loopback listener reported unreachable (%v); "+
			"the old %%s:%%d formatting made this dial fail", rtt)
	}
}

// --- Regression for issue #18: DialAnyPeer false positives & fan-out cost ---
// A completed peer tunnel handshake is not proof the peer reached the
// target: dialDedicated returns right after writing the trojan header, so a
// peer whose outbound dial failed produced a dead-on-arrival conn that
// DialAnyPeer happily returned as a "successful" relay. The user saw an
// instant EOF and the negative cache never filled. verifyFallbackConn must
// reject such conns while never eating real data.

func TestVerifyFallbackConn(t *testing.T) {
	t.Run("silent conn passes and deadline is cleared", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		conn, err := verifyFallbackConn(client, 150*time.Millisecond)
		if err != nil {
			t.Fatalf("silent conn rejected: %v", err)
		}
		// The probe deadline must be cleared: data arriving long after the
		// verify window must still be readable.
		go func() {
			time.Sleep(300 * time.Millisecond)
			server.Write([]byte("late"))
		}()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read after verify window failed (deadline not cleared?): %v", err)
		}
		if string(buf) != "late" {
			t.Fatalf("got %q, want %q", buf, "late")
		}
	})

	t.Run("immediate close is rejected", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close() // peer "couldn't reach the target"
			}
		}()

		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := verifyFallbackConn(c, time.Second); err == nil {
			t.Fatal("dead-on-arrival conn accepted; want rejection")
		}
	})

	t.Run("byte read during verify is preserved", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			c.Write([]byte("hello"))
			time.Sleep(2 * time.Second)
		}()

		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		conn, err := verifyFallbackConn(c, time.Second)
		if err != nil {
			t.Fatalf("conn with immediate data rejected: %v", err)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf) != "hello" {
			t.Fatalf("got %q, want %q (verify ate the first byte)", buf, "hello")
		}
	})
}

// deadOnArrivalTLSListener simulates a peer that completes the tunnel
// handshake but cannot reach the target: it finishes TLS, drains the trojan
// header (so the client's write succeeds), then closes the connection.
func deadOnArrivalTLSListener(t *testing.T) net.Listener {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dead-peer.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				buf := make([]byte, 512)
				c.Read(buf) // drain trojan header, then drop
			}(c)
		}
	}()
	return ln
}

func TestRouterDialAnyPeerDeadOnArrival(t *testing.T) {
	ln := deadOnArrivalTLSListener(t)
	defer ln.Close()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("bad listener addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:          true,
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		RelayThreshold:   50,
		LatencyThreshold: 100,
		Peers: []PeerConfig{{
			Name: "dead-peer", Host: "127.0.0.1", Port: port, Password: "x",
			SSL: PeerSSLConfig{Verify: boolPtr(false)},
		}},
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	dialers := make(map[string]*PeerDialer)
	pd, err := NewPeerDialer(ctx, cfg.Peers[0])
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	dialers["dead-peer"] = pd
	prober := NewProber(ctx, cfg, rt, dialers, metrics)
	prober.peerAlive.Store("dead-peer", peerAliveInfo{Alive: true})

	cr := &ClusterRouter{
		enabled:     true,
		routeTable:  rt,
		prober:      prober,
		peerDialers: dialers,
		matcher:     NewTargetMatcher(nil),
		metrics:     metrics,
		localName:   "local",
		cfg:         cfg,
	}

	addr := &tunnel.Address{
		IP:          net.ParseIP("1.2.3.4"),
		Port:        80,
		AddressType: tunnel.IPv4,
	}

	start := time.Now()
	conn, _, err := cr.DialAnyPeer(addr)
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		t.Fatal("dead-on-arrival peer conn returned as a successful relay; " +
			"the user would see an instant EOF")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fallback took %v; per-peer dial must be bounded well under the old 10s", elapsed)
	}

	// The failure must populate the negative cache so the next connection
	// short-circuits instead of repeating the handshake fan-out.
	start = time.Now()
	_, _, err = cr.DialAnyPeer(addr)
	elapsed = time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "suppressed") {
		t.Fatalf("expected suppressed error on second call, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("negative cache did not short-circuit (took %v)", elapsed)
	}
}

// --- Regression for issue #18: slow-but-successful dials got no urgent probe ---
// A newly registered slow target used to sit unprobed until the next full
// probe cycle (>=60s): no peer measurements AND no local route-table entry,
// so BestExit stayed blind. Registration must publish the local RTT and
// kick an urgent peer probe.

func TestProberSlowSuccessPublishesLocalRTTAndProbesUrgently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 100,
	}
	rt := NewRouteTable("local", 50)
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), NewClusterMetrics())

	p.RegisterSlowTarget("8.8.8.8", 443, 2*time.Second, false)

	// The local entry must exist immediately with the measured RTT, so
	// BestExit has a baseline to compare urgent peer probes against.
	found := false
	for _, pl := range rt.GetAllEntries()["8.8.8.8:443"] {
		if pl.PeerName == "local" {
			found = true
			if !pl.Available {
				t.Fatal("slow-but-successful dial marked local unavailable")
			}
			if pl.RTT <= 0 {
				t.Fatalf("local RTT not published, got %v", pl.RTT)
			}
		}
	}
	if !found {
		t.Fatal("slow-but-successful registration did not create a local route entry")
	}
}

// --- Regression for issue #19: openMuxStream serialized callers behind the handshake ---
// The session build (TCP+TLS+WS+trojan+smux, up to the full dial timeout)
// ran while holding muxMu. N concurrent relays to a slow peer cost
// N × handshake timeout, and callers that could have reused an existing
// session with spare capacity were stuck behind the lock too. The build
// must run outside the critical section with a single-flight placeholder.

func TestOpenMuxStreamSlowBuildNotSerialized(t *testing.T) {
	// TCP listener that accepts but never completes TLS: the client
	// handshake hangs until its own timeout expires.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var heldMu sync.Mutex
	var held []net.Conn // keep refs so no finalizer closes them mid-test
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, c)
			heldMu.Unlock()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd, err := NewPeerDialer(ctx, PeerConfig{
		Name: "slow-build", Host: "127.0.0.1", Port: port, Password: "x",
		SSL: PeerSSLConfig{Verify: boolPtr(false)},
		Mux: PeerMuxConfig{Enabled: true, Concurrency: 4},
	})
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	defer pd.Close()

	const callers = 5
	const buildTimeout = 400 * time.Millisecond
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := pd.openMuxStream(buildTimeout)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d: expected build failure against a hanging peer", i)
		}
	}
	// Old code serialized 5 handshakes under muxMu: >= 5×400ms = 2s.
	// Single-flight: one build (~400ms) and every waiter fails with it.
	if elapsed > 1400*time.Millisecond {
		t.Fatalf("%d concurrent openMuxStream calls took %v; the handshake is still serialized behind muxMu", callers, elapsed)
	}
}

// muxTestTLSServer is a minimal trojan-mux peer: TLS, drain the 73-byte
// trojan mux header, then serve smux streams (accepted and held open).
// The returned counter tracks established mux sessions.
func muxTestTLSServer(t *testing.T) (net.Listener, *int32) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mux-peer.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	var sessions int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Trojan mux header: 56 hex hash + CRLF + CMD + ATYP(3) +
				// len + "MUX_CONN" + port(2) + CRLF = 73 bytes.
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.ReadFull(c, make([]byte, 73)); err != nil {
					return
				}
				c.SetReadDeadline(time.Time{})
				atomic.AddInt32(&sessions, 1)
				scfg := smux.DefaultConfig()
				scfg.KeepAliveInterval = 15 * time.Second
				scfg.KeepAliveTimeout = 60 * time.Second
				sess, err := smux.Server(c, scfg)
				if err != nil {
					return
				}
				for {
					s, err := sess.AcceptStream()
					if err != nil {
						return
					}
					go func(s *smux.Stream) {
						io.Copy(io.Discard, s) // hold the stream open
					}(s)
				}
			}(c)
		}
	}()
	return ln, &sessions
}

func TestOpenMuxStreamSingleFlightBuildsMinimalSessions(t *testing.T) {
	ln, sessions := muxTestTLSServer(t)
	defer ln.Close()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd, err := NewPeerDialer(ctx, PeerConfig{
		Name: "mux-peer", Host: "127.0.0.1", Port: port, Password: "x",
		SSL: PeerSSLConfig{Verify: boolPtr(false)},
		Mux: PeerMuxConfig{Enabled: true, Concurrency: 4},
	})
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	defer pd.Close()

	// 8 concurrent callers, capacity 4 per session: exactly 2 sessions
	// must be built — waiters share the in-flight build instead of each
	// starting their own handshake.
	const callers = 8
	streams := make([]*smux.Stream, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := pd.openMuxStream(5 * time.Second)
			streams[i], errs[i] = s, err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: openMuxStream failed: %v", i, err)
		}
		defer streams[i].Close()
	}

	// The server increments its session counter after draining the trojan
	// header, which is not strictly ordered against the client's build
	// returning — poll briefly instead of reading it race-prone once.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(sessions) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(sessions); got != 2 {
		t.Fatalf("server saw %d mux sessions, want exactly 2 (single-flight violated: duplicate handshakes)", got)
	}
	pd.muxMu.Lock()
	clientSessions := len(pd.muxSessions)
	pd.muxMu.Unlock()
	if clientSessions != 2 {
		t.Fatalf("client holds %d mux sessions, want 2", clientSessions)
	}
}

// --- Regression for issue #21: stale eviction clobbered concurrent refreshes ---
// evictStaleTargets judged a snapshot from Range, then deleted blindly by
// key (LoadAndDelete). A RegisterSlowTarget refresh landing between the
// snapshot and the delete was thrown away and the count decremented for a
// live entry. Eviction must only remove the exact pointer it judged stale.

func TestEvictEntryDoesNotClobberRefreshedTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewProber(ctx, &Config{
		NodeName: "local", ProbeInterval: 120, ProbeTimeout: 3000, LatencyThreshold: 100,
	}, NewRouteTable("local", 50), make(map[string]*PeerDialer), NewClusterMetrics())

	key := "9.9.9.9:443"
	// Seed a stale entry: the snapshot an eviction pass would judge on.
	stale := &DynamicTarget{
		Host: "9.9.9.9", Port: 443,
		FirstSeenAt: time.Now().Add(-time.Hour),
		LastSeenAt:  time.Now().Add(-31 * time.Minute),
	}
	p.dynamicTargets.Store(key, stale)
	atomic.StoreInt32(&p.dynamicCount, 1)

	// A concurrent refresh publishes a NEW pointer under the same key
	// (this is exactly what RegisterSlowTarget does).
	p.RegisterSlowTarget("9.9.9.9", 443, 200*time.Millisecond, false)

	// Eviction judged on the stale snapshot must not delete the refresh.
	if p.evictEntry(key, stale) {
		t.Fatal("evictEntry deleted a target refreshed after the snapshot (blind LoadAndDelete regression)")
	}
	if _, ok := p.dynamicTargets.Load(key); !ok {
		t.Fatal("refreshed target disappeared from dynamicTargets")
	}
	if got := p.DynamicTargetCount(); got != 1 {
		t.Fatalf("dynamic target count = %d, want 1 (count drifted from map)", got)
	}

	// Eviction against the current pointer must still succeed.
	cur, _ := p.dynamicTargets.Load(key)
	if !p.evictEntry(key, cur) {
		t.Fatal("evictEntry failed to remove the entry it was pointed at")
	}
	if _, ok := p.dynamicTargets.Load(key); ok {
		t.Fatal("entry survived eviction of its current pointer")
	}
	if got := p.DynamicTargetCount(); got != 0 {
		t.Fatalf("dynamic target count = %d, want 0", got)
	}
}

// --- Regression for issue #21: fallback negative cache never shrank ---
// Expired entries were only observed lazily when DialAnyPeer was called
// again for the same key; targets that never reappeared leaked map entries
// for the lifetime of the process.

func TestSweepExpiredNegCache(t *testing.T) {
	cr := &ClusterRouter{enabled: true}
	cr.fallbackNegCache.Store("dead.target:443", time.Now().Add(-time.Second))
	cr.fallbackNegCache.Store("live.target:443", time.Now().Add(time.Minute))

	if n := cr.sweepExpiredNegCache(); n != 1 {
		t.Fatalf("sweep removed %d entries, want 1", n)
	}
	if _, ok := cr.fallbackNegCache.Load("dead.target:443"); ok {
		t.Fatal("expired negative-cache entry survived the sweep")
	}
	if _, ok := cr.fallbackNegCache.Load("live.target:443"); !ok {
		t.Fatal("still-valid negative-cache entry was swept")
	}
	if n := cr.sweepExpiredNegCache(); n != 0 {
		t.Fatalf("second sweep removed %d entries, want 0", n)
	}
}

// --- Regression for issue #21: globalRouter registration raced and stuck ---
// The singleton was a plain variable written under sync.Once: HTTP API
// readers raced the write, and a router created after the first (e.g. on
// config reload) was silently never registered. Registration must be
// atomic, last-created-wins, and Stop() must only unregister itself.

func TestGlobalRouterLastCreatedWinsAndStopUnregisters(t *testing.T) {
	orig := globalRouter.Load()
	defer globalRouter.Store(orig)

	r1 := buildSnapshotRouter(t)
	r2 := buildSnapshotRouter(t)

	// Readers hammering GlobalRouter() give -race something to observe
	// against the concurrent Store/CompareAndSwap below.
	stopReaders := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_ = GlobalRouter()
				}
			}
		}()
	}

	globalRouter.Store(r1)
	globalRouter.Store(r2) // as if NewClusterRouter ran again after a reload
	if GlobalRouter() != r2 {
		t.Fatal("second router did not replace the first (last-created must win)")
	}

	r1.Stop() // stopping a superseded router must not unregister r2
	if GlobalRouter() != r2 {
		t.Fatal("Stop on a superseded router unregistered the current one")
	}

	r2.Stop()
	if GlobalRouter() != nil {
		t.Fatal("Stop on the current router did not unregister it")
	}

	close(stopReaders)
	wg.Wait()
}

// --- Regression for issue #22: local/private targets were relayable ---
// Nothing exempted loopback/RFC1918/link-local targets from relay: with
// force_relay (or a route table poisoned by an urgent probe), "connect to
// my NAS at 192.168.1.10" became "ask a foreign peer to dial ITS OWN
// 192.168.1.10". All three relay paths must refuse such targets.

func buildLocalExemptionRouter(t *testing.T) (*ClusterRouter, *ClusterMetrics) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := &Config{
		Enabled:          true,
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		RelayThreshold:   50,
		LatencyThreshold: 100,
		Peers: []PeerConfig{
			{Name: "peer-x", Host: "127.0.0.1", Port: 19999, Password: "x"},
		},
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	dialers := make(map[string]*PeerDialer)
	pd, err := NewPeerDialer(ctx, cfg.Peers[0])
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	dialers["peer-x"] = pd
	prober := NewProber(ctx, cfg, rt, dialers, metrics)
	cr := &ClusterRouter{
		ctx:         ctx,
		enabled:     true,
		routeTable:  rt,
		prober:      prober,
		peerDialers: dialers,
		matcher:     NewTargetMatcher(nil),
		metrics:     metrics,
		localName:   "local",
		cfg:         cfg,
	}
	return cr, metrics
}

func TestIsLocalTarget(t *testing.T) {
	// Swap in a deterministic resolver for the domain cases; unique fake
	// hostnames keep the 30s DNS cache from cross-contaminating tests.
	origResolve := resolveIPFunc
	defer func() { resolveIPFunc = origResolve }()
	resolveIPFunc = func(_ context.Context, host string) (string, error) {
		switch host {
		case "nas.internal.test":
			return "192.168.0.1", nil
		case "public.example.test":
			return "93.184.216.34", nil
		}
		return "", &net.DNSError{Err: "no such host", Name: host}
	}

	ipAddr := func(s string) *tunnel.Address {
		ip := net.ParseIP(s)
		at := tunnel.IPv4
		if ip.To4() == nil {
			at = tunnel.IPv6
		}
		return &tunnel.Address{IP: ip, Port: 443, AddressType: at}
	}

	cases := []struct {
		name string
		addr *tunnel.Address
		want bool
	}{
		{"ipv4 loopback", ipAddr("127.0.0.1"), true},
		{"ipv6 loopback", ipAddr("::1"), true},
		{"private 192.168", ipAddr("192.168.1.10"), true},
		{"private 10", ipAddr("10.8.0.1"), true},
		{"private 172.16", ipAddr("172.16.5.5"), true},
		{"link-local", ipAddr("169.254.169.254"), true},
		{"unspecified", ipAddr("0.0.0.0"), true},
		{"ipv6 ULA", ipAddr("fd00::1"), true},
		{"public ipv4", ipAddr("8.8.8.8"), false},
		{"public ipv6", ipAddr("2001:4860:4860::8888"), false},
		{"mux magic domain", &tunnel.Address{DomainName: "MUX_CONN", Port: 0, AddressType: tunnel.DomainName}, false},
		{"domain resolving private", &tunnel.Address{DomainName: "nas.internal.test", Port: 443, AddressType: tunnel.DomainName}, true},
		{"domain resolving public", &tunnel.Address{DomainName: "public.example.test", Port: 443, AddressType: tunnel.DomainName}, false},
		{"unresolvable domain", &tunnel.Address{DomainName: "gone.example.test", Port: 443, AddressType: tunnel.DomainName}, false},
	}
	for _, c := range cases {
		if got := isLocalTarget(c.addr); got != c.want {
			t.Errorf("isLocalTarget(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRouterDialConnExemptsLocalTargets(t *testing.T) {
	cr, metrics := buildLocalExemptionRouter(t)

	// Poison the route table so the relay WOULD fire without the
	// exemption: local unreachable, peer fast.
	cr.routeTable.Update("192.168.1.100:443", "local", -1)
	cr.routeTable.Update("192.168.1.100:443", "peer-x", 10*time.Millisecond)

	addr := &tunnel.Address{
		IP:          net.ParseIP("192.168.1.100"),
		Port:        443,
		AddressType: tunnel.IPv4,
	}

	conn, node, err := cr.DialConn(addr)
	if conn != nil {
		conn.Close()
		t.Fatal("private target was relayed to a peer")
	}
	if node != "local" || err != nil {
		t.Fatalf("DialConn = (%q, %v), want (\"local\", nil)", node, err)
	}

	// Same under force_relay: the exemption must run before that branch.
	cr.cfg.ForceRelay = true
	conn, node, err = cr.DialConn(addr)
	if conn != nil {
		conn.Close()
		t.Fatal("private target was force-relayed to a peer")
	}
	if node != "local" || err != nil {
		t.Fatalf("force-relay DialConn = (%q, %v), want (\"local\", nil)", node, err)
	}

	// No peer dial may ever be attempted: both relays and fallbacks stay 0.
	if stats := metrics.Snapshot(); stats.TotalRelays != 0 || stats.TotalFallbacks != 0 {
		t.Fatalf("peer dials attempted for a private target: relays=%d fallbacks=%d",
			stats.TotalRelays, stats.TotalFallbacks)
	}
}

func TestDialAnyPeerRefusesLocalTarget(t *testing.T) {
	cr, metrics := buildLocalExemptionRouter(t)
	cr.prober.peerAlive.Store("peer-x", peerAliveInfo{Alive: true})

	addr := &tunnel.Address{
		IP:          net.ParseIP("127.0.0.1"),
		Port:        80,
		AddressType: tunnel.IPv4,
	}
	_, _, err := cr.DialAnyPeer(addr)
	if err == nil {
		t.Fatal("DialAnyPeer accepted a loopback target")
	}
	if !strings.Contains(err.Error(), "local/private") {
		t.Fatalf("unexpected error %q; want the local/private refusal", err)
	}
	if stats := metrics.Snapshot(); stats.TotalFallbacks != 0 {
		t.Fatalf("peer handshakes attempted for a loopback target: %d", stats.TotalFallbacks)
	}
}

// --- Regression for issue #23: sentinel gain polluted AvgGainMs ---
// Block-bypass relays are recorded with the unreachableGain sentinel (10s)
// — not a measured improvement. Averaging it into totalGainMs inflated
// AvgGainMs by orders of magnitude; bypass relays must be counted
// separately and excluded from the average.

func TestRecordRelayBypassExcludedFromAvgGain(t *testing.T) {
	m := NewClusterMetrics()
	m.RecordRelay("peer-a", 100*time.Millisecond)
	m.RecordRelay("peer-a", 200*time.Millisecond)
	m.RecordRelay("peer-a", unreachableGain) // block-bypass sentinel

	s := m.Snapshot()
	if s.TotalRelays != 3 {
		t.Fatalf("TotalRelays = %d, want 3", s.TotalRelays)
	}
	if s.BypassRelays != 1 {
		t.Fatalf("BypassRelays = %d, want 1", s.BypassRelays)
	}
	// (100+200)/2. Old code: (100+200+10000)/3 ≈ 3433.
	if s.AvgGainMs != 150 {
		t.Fatalf("AvgGainMs = %.1f, want 150 (sentinel leaked into the average)", s.AvgGainMs)
	}

	// A bypass-only router must report 0, not divide by zero.
	m2 := NewClusterMetrics()
	m2.RecordRelay("peer-b", unreachableGain)
	if s2 := m2.Snapshot(); s2.AvgGainMs != 0 || s2.BypassRelays != 1 {
		t.Fatalf("bypass-only snapshot = %+v, want AvgGainMs 0 / BypassRelays 1", s2)
	}
}

// --- Regression for issue #23: trojan header wrote corrupt addresses silently ---
// A mismatched AddressType/IP made To4()/To16() return nil and buf.Write
// emitted zero address bytes with no error; the peer parsed the port and
// CRLF at the wrong offsets. Validation must reject before any write.

func TestWriteTrojanHeaderRejectsMalformedAddress(t *testing.T) {
	pd := &PeerDialer{name: "test", passHash: hexSHA224("pw")}
	longDomain := strings.Repeat("a", 256)

	bad := []struct {
		name string
		addr *tunnel.Address
	}{
		{"IPv4 type with IPv6 IP", &tunnel.Address{IP: net.ParseIP("2001:db8::1"), Port: 443, AddressType: tunnel.IPv4}},
		{"IPv4 type with nil IP", &tunnel.Address{Port: 443, AddressType: tunnel.IPv4}},
		{"IPv6 type with nil IP", &tunnel.Address{Port: 443, AddressType: tunnel.IPv6}},
		{"empty domain", &tunnel.Address{DomainName: "", Port: 443, AddressType: tunnel.DomainName}},
		{"domain over 255 bytes", &tunnel.Address{DomainName: longDomain, Port: 443, AddressType: tunnel.DomainName}},
		{"unknown address type", &tunnel.Address{IP: net.ParseIP("1.2.3.4"), Port: 443, AddressType: tunnel.AddressType(99)}},
	}
	for _, c := range bad {
		fake := &fakeWriteConn{buf: &bytes.Buffer{}}
		if err := pd.writeTrojanHeaderCmd(fake, c.addr, cmdConnect); err == nil {
			t.Errorf("%s: header written without error", c.name)
		}
		if fake.buf.Len() != 0 {
			t.Errorf("%s: %d bytes written before rejection; want 0", c.name, fake.buf.Len())
		}
	}

	good := []struct {
		name     string
		addr     *tunnel.Address
		wantSize int // 56 hash + 2 CRLF + 1 cmd + addr + 2 port + 2 CRLF
	}{
		{"valid IPv4", &tunnel.Address{IP: net.ParseIP("1.2.3.4"), Port: 443, AddressType: tunnel.IPv4}, 56 + 2 + 1 + 1 + 4 + 2 + 2},
		{"valid IPv6", &tunnel.Address{IP: net.ParseIP("2001:db8::1"), Port: 443, AddressType: tunnel.IPv6}, 56 + 2 + 1 + 1 + 16 + 2 + 2},
		{"valid domain", &tunnel.Address{DomainName: "example.com", Port: 443, AddressType: tunnel.DomainName}, 56 + 2 + 1 + 1 + 1 + 11 + 2 + 2},
	}
	for _, c := range good {
		fake := &fakeWriteConn{buf: &bytes.Buffer{}}
		if err := pd.writeTrojanHeaderCmd(fake, c.addr, cmdConnect); err != nil {
			t.Errorf("%s: valid address rejected: %v", c.name, err)
		}
		if fake.buf.Len() != c.wantSize {
			t.Errorf("%s: wrote %d bytes, want %d", c.name, fake.buf.Len(), c.wantSize)
		}
	}
}

// --- Regression for issue #23: probeDirect bypassed the shared DNS cache ---
// Every probe cycle re-resolved domain targets through net.Dial's own
// lookup, outside the 30s cache and single-flight merging, and the lookup
// time leaked into the measured connect RTT.

func TestProbeDirectUsesSharedResolver(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	origResolve := resolveIPFunc
	defer func() { resolveIPFunc = origResolve }()
	var resolves int32
	resolveIPFunc = func(_ context.Context, host string) (string, error) {
		if host == "probe-cache.test" {
			atomic.AddInt32(&resolves, 1)
			return "127.0.0.1", nil
		}
		return "", &net.DNSError{Err: "no such host", Name: host}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewProber(ctx, &Config{NodeName: "local", ProbeTimeout: 3000, LatencyThreshold: 100},
		NewRouteTable("local", 50), make(map[string]*PeerDialer), NewClusterMetrics())

	// Three probes of the same domain: the shared cache must collapse them
	// into a single resolution, and all three must reach the listener via
	// the resolved IP (the old net.Dial path would have failed outright on
	// the unresolvable .test name).
	for i := 0; i < 3; i++ {
		if rtt := p.probeDirect(ProbeTarget{Host: "probe-cache.test", Port: port}); rtt < 0 {
			t.Fatalf("probe %d: probeDirect reported unreachable despite cached resolution", i)
		}
	}
	if got := atomic.LoadInt32(&resolves); got != 1 {
		t.Fatalf("resolver called %d times for 3 probes, want 1 (DNS cache bypassed)", got)
	}
}
