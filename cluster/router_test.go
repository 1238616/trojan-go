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
		enabled: true,
		matcher: NewTargetMatcher([]string{"cidr:149.154.160.0/20"}),
		routeTable: NewRouteTable("local", 50),
		metrics: NewClusterMetrics(),
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
