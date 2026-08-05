package cluster

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
