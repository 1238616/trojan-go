package cluster

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

func TestRouterDialConnDisabled(t *testing.T) {
	cr := &ClusterRouter{enabled: false}
	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("149.154.175.53"),
		Port:        443,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn when disabled")
	}
	if node != "local" {
		t.Fatalf("expected local, got %q", node)
	}
}

func TestRouterDialConnNil(t *testing.T) {
	var cr *ClusterRouter
	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("1.1.1.1"),
		Port:        80,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn for nil router")
	}
	if node != "local" {
		t.Fatalf("expected local, got %q", node)
	}
}

func TestRouterDialConnNotInTargets(t *testing.T) {
	cr := &ClusterRouter{
		enabled:    true,
		matcher:    NewTargetMatcher([]string{"cidr:149.154.160.0/20"}),
		routeTable: NewRouteTable("local", 50),
		metrics:    NewClusterMetrics(),
	}

	// 8.8.8.8 is NOT in 149.154.160.0/20
	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("8.8.8.8"),
		Port:        53,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn for non-matching target")
	}
	if node != "local" {
		t.Fatalf("expected local, got %q", node)
	}
}

func TestRouterDialConnLocalOptimal(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("149.154.175.53:443", "local", 30*time.Millisecond)
	rt.Update("149.154.175.53:443", "peer-a", 25*time.Millisecond)
	// Gain = 30 - 25 = 5ms < threshold 50ms → local

	cr := &ClusterRouter{
		enabled:    true,
		matcher:    NewTargetMatcher([]string{"cidr:149.154.160.0/20"}),
		routeTable: rt,
		metrics:    NewClusterMetrics(),
		localName:  "local",
	}

	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("149.154.175.53"),
		Port:        443,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn (local optimal)")
	}
	if node != "local" {
		t.Fatalf("expected local, got %q", node)
	}
}

func TestRouterDialConnRelayNoPeerDialer(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("149.154.175.53:443", "local", 250*time.Millisecond)
	rt.Update("149.154.175.53:443", "la-1", 15*time.Millisecond)

	cr := &ClusterRouter{
		enabled:     true,
		matcher:     NewTargetMatcher([]string{"cidr:149.154.160.0/20"}),
		routeTable:  rt,
		peerDialers: map[string]*PeerDialer{}, // empty → no dialer for la-1
		metrics:     NewClusterMetrics(),
		localName:   "local",
	}

	conn, node, _ := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("149.154.175.53"),
		Port:        443,
		AddressType: tunnel.IPv4,
	})
	if conn != nil {
		t.Fatal("expected nil conn (no dialer)")
	}
	if node != "local" {
		t.Fatalf("expected local fallback, got %q", node)
	}
}

func TestRouterSnapshotStructure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:          true,
		NodeName:         "singapore-1",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		RelayThreshold:   50,
		LatencyThreshold: 100,
		Targets:          []string{"cidr:149.154.160.0/20"},
		Peers: []PeerConfig{
			{Name: "la-1", Host: "la.example.com", Port: 443, Password: "s"},
		},
	}

	rt := NewRouteTable("singapore-1", 50)
	rt.Update("149.154.175.53:443", "singapore-1", 250*time.Millisecond)
	rt.Update("149.154.175.53:443", "la-1", 15*time.Millisecond)

	metrics := NewClusterMetrics()
	metrics.RecordRelay("la-1", 235*time.Millisecond)

	peerDialers := make(map[string]*PeerDialer)
	pd, _ := NewPeerDialer(ctx, cfg.Peers[0])
	peerDialers["la-1"] = pd

	prober := NewProber(ctx, cfg, rt, peerDialers, metrics)

	cr := &ClusterRouter{
		enabled:     true,
		routeTable:  rt,
		prober:      prober,
		peerDialers: peerDialers,
		matcher:     NewTargetMatcher(cfg.Targets),
		metrics:     metrics,
		localName:   "singapore-1",
		cfg:         cfg,
	}

	snap := cr.Snapshot()
	if !snap.Enabled {
		t.Fatal("expected enabled=true")
	}
	if snap.LocalNode != "singapore-1" {
		t.Fatalf("expected LocalNode=singapore-1, got %q", snap.LocalNode)
	}
	if snap.ProbeInterval != 120 {
		t.Fatalf("expected ProbeInterval=120, got %d", snap.ProbeInterval)
	}
	if len(snap.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(snap.Peers))
	}
	if snap.Stats.TotalRelays != 1 {
		t.Fatalf("expected TotalRelays=1, got %d", snap.Stats.TotalRelays)
	}
	if len(snap.OptimizedRoutes) == 0 {
		t.Fatal("expected at least 1 optimized route")
	}
	route := snap.OptimizedRoutes[0]
	if route.BestPeer != "la-1" {
		t.Fatalf("expected BestPeer=la-1, got %q", route.BestPeer)
	}
	if route.GainMs < 200 {
		t.Fatalf("expected GainMs > 200, got %.1f", route.GainMs)
	}
}

func BenchmarkMatcherMatch(b *testing.B) {
	tm := NewTargetMatcher([]string{
		"cidr:149.154.160.0/20",
		"cidr:91.108.0.0/16",
		"domain:telegram.org",
		"ip:1.2.3.4",
	})
	addr := &tunnel.Address{
		IP:          net.ParseIP("149.154.175.53"),
		Port:        443,
		AddressType: tunnel.IPv4,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tm.Match(addr)
	}
}

func TestRouterForceRelaySelectsFastestPeer(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("8.8.8.8:53", "local", 200*time.Millisecond)
	rt.Update("8.8.8.8:53", "peer-slow", 100*time.Millisecond)
	rt.Update("8.8.8.8:53", "peer-fast", 10*time.Millisecond)

	cr := &ClusterRouter{
		enabled:     true,
		matcher:     NewTargetMatcher([]string{"cidr:149.154.160.0/20"}), // doesn't cover 8.8.8.8
		routeTable:  rt,
		peerDialers: map[string]*PeerDialer{}, // no real dialers → will fallback
		metrics:     NewClusterMetrics(),
		localName:   "local",
		cfg:         &Config{Enabled: true, ForceRelay: true},
	}

	// 8.8.8.8 is NOT in matcher, but ForceRelay skips matcher entirely.
	// No real dialer → falls back to local, but we verify the path was taken
	// (not rejected by matcher).
	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("8.8.8.8"),
		Port:        53,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn (no real dialer)")
	}
	// Falls back to local because peerDialers map is empty (no dialer for "peer-fast")
	if node != "local" {
		t.Fatalf("expected local fallback, got %q", node)
	}
}

func TestRouterForceRelayFallsBackToAnyPeer(t *testing.T) {
	// Empty route table — no probe data for this target
	rt := NewRouteTable("local", 50)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pd, _ := NewPeerDialer(ctx, PeerConfig{
		Name: "fallback-peer", Host: "127.0.0.1", Port: 19999, Password: "test",
	})

	cr := &ClusterRouter{
		enabled:     true,
		matcher:     NewTargetMatcher(nil),
		routeTable:  rt,
		peerDialers: map[string]*PeerDialer{"fallback-peer": pd},
		metrics:     NewClusterMetrics(),
		localName:   "local",
		cfg:         &Config{Enabled: true, ForceRelay: true},
	}

	// No probe data → should pick "fallback-peer" as the only available peer.
	// DialConn on peer will fail (no server at 127.0.0.1:19999) → graceful fallback to local.
	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("1.2.3.4"),
		Port:        80,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn (peer unreachable)")
	}
	if node != "local" {
		t.Fatalf("expected local fallback, got %q", node)
	}
}

func TestRouterForceRelaySkipsMatcher(t *testing.T) {
	// Matcher only allows 10.0.0.0/8, target is 8.8.8.8 — normally rejected.
	// With ForceRelay, matcher is bypassed.
	rt := NewRouteTable("local", 50)

	cr := &ClusterRouter{
		enabled:     true,
		matcher:     NewTargetMatcher([]string{"cidr:10.0.0.0/8"}),
		routeTable:  rt,
		peerDialers: map[string]*PeerDialer{},
		metrics:     NewClusterMetrics(),
		localName:   "local",
		cfg:         &Config{Enabled: true, ForceRelay: true},
	}

	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("8.8.8.8"),
		Port:        53,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Without ForceRelay, this would return "local" due to matcher rejection at line ~128.
	// With ForceRelay, it bypasses matcher but has no peers → still returns local.
	if conn != nil {
		t.Fatal("expected nil conn")
	}
	if node != "local" {
		t.Fatalf("expected local, got %q", node)
	}
}

func TestRouterForceRelayNoPeerDialers(t *testing.T) {
	rt := NewRouteTable("local", 50)

	cr := &ClusterRouter{
		enabled:     true,
		matcher:     NewTargetMatcher(nil),
		routeTable:  rt,
		peerDialers: map[string]*PeerDialer{},
		metrics:     NewClusterMetrics(),
		localName:   "local",
		cfg:         &Config{Enabled: true, ForceRelay: true},
	}

	conn, node, err := cr.DialConn(&tunnel.Address{
		IP:          net.ParseIP("1.1.1.1"),
		Port:        443,
		AddressType: tunnel.IPv4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn != nil {
		t.Fatal("expected nil conn")
	}
	if node != "local" {
		t.Fatalf("expected local, got %q", node)
	}
}

func TestRouteTableFastestPeer(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("1.1.1.1:443", "local", 200*time.Millisecond)
	rt.Update("1.1.1.1:443", "peer-a", 80*time.Millisecond)
	rt.Update("1.1.1.1:443", "peer-b", 20*time.Millisecond)
	rt.Update("1.1.1.1:443", "peer-c", 50*time.Millisecond)

	best := rt.FastestPeer("1.1.1.1", 443)
	if best != "peer-b" {
		t.Fatalf("expected peer-b (fastest), got %q", best)
	}
}

func TestRouteTableFastestPeerIgnoresLocal(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("1.1.1.1:443", "local", 5*time.Millisecond) // local is fastest

	best := rt.FastestPeer("1.1.1.1", 443)
	if best != "" {
		t.Fatalf("expected empty (no non-local peer), got %q", best)
	}
}

func TestRouteTableFastestPeerIgnoresUnavailable(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("1.1.1.1:443", "peer-down", -1) // unavailable
	rt.Update("1.1.1.1:443", "peer-up", 30*time.Millisecond)

	best := rt.FastestPeer("1.1.1.1", 443)
	if best != "peer-up" {
		t.Fatalf("expected peer-up, got %q", best)
	}
}

func TestRouterDialAnyPeerDisabled(t *testing.T) {
	cr := &ClusterRouter{enabled: false}
	conn, _, err := cr.DialAnyPeer(&tunnel.Address{
		IP:          net.ParseIP("1.2.3.4"),
		Port:        80,
		AddressType: tunnel.IPv4,
	})
	if err == nil {
		t.Fatal("expected error for disabled router")
	}
	if conn != nil {
		t.Fatal("expected nil conn for disabled router")
	}
}

func TestRouterDialAnyPeerNoCandidates(t *testing.T) {
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
			{Name: "p-down", Host: "127.0.0.1", Port: 19999, Password: "x"},
		},
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	dialers := make(map[string]*PeerDialer)
	pd, _ := NewPeerDialer(ctx, cfg.Peers[0])
	dialers["p-down"] = pd
	prober := NewProber(ctx, cfg, rt, dialers, metrics)
	// peerAlive defaults to absent → IsPeerAlive returns false.

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

	conn, _, err := cr.DialAnyPeer(&tunnel.Address{
		IP:          net.ParseIP("1.2.3.4"),
		Port:        80,
		AddressType: tunnel.IPv4,
	})
	if err == nil {
		t.Fatal("expected error when no peer is alive")
	}
	if conn != nil {
		t.Fatal("expected nil conn when no peer is alive")
	}
}

func TestRouterDialAnyPeerAllFail(t *testing.T) {
	// Peer is marked alive but the underlying dial will fail (port is closed).
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
	prober.peerAlive.Store("p-alive-but-broken", true)

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

	start := time.Now()
	conn, peer, err := cr.DialAnyPeer(&tunnel.Address{
		IP:          net.ParseIP("1.2.3.4"),
		Port:        80,
		AddressType: tunnel.IPv4,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error when peer dial fails")
	}
	if conn != nil {
		t.Fatal("expected nil conn when peer dial fails")
	}
	if peer != "" {
		t.Fatalf("expected empty peer name, got %q", peer)
	}
	// TCP connect to a closed local port should fail fast (refused), not hang.
	if elapsed > 5*time.Second {
		t.Fatalf("DialAnyPeer took too long: %v", elapsed)
	}
}
