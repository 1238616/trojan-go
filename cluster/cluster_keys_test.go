package cluster

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

// --- Regression for issue #5: route-table write keys vs read keys ---
// The prober wrote "host:port" entries while the router looked the table up
// with bare resolved IPs, and CIDR targets were skipped entirely by the
// prober. Together that made the cluster feature inert under the README
// example configuration.

func TestCIDRRepresentative(t *testing.T) {
	_, ipnet, _ := net.ParseCIDR("149.154.160.0/20")
	if got := cidrRepresentative(ipnet).String(); got != "149.154.160.1" {
		t.Fatalf("expected 149.154.160.1, got %s", got)
	}

	_, ipnet, _ = net.ParseCIDR("10.0.0.255/32")
	if got := cidrRepresentative(ipnet).String(); got != "10.0.0.255" {
		t.Fatalf("/32 must probe the address itself, got %s", got)
	}

	_, ipnet, _ = net.ParseCIDR("2001:db8::/32")
	if got := cidrRepresentative(ipnet).String(); got != "2001:db8::1" {
		t.Fatalf("expected 2001:db8::1, got %s", got)
	}
}

func TestProberParsesStaticTargetRules(t *testing.T) {
	cfg := &Config{
		Enabled:       true,
		NodeName:      "sg-1",
		ProbeInterval: 120,
		ProbeTimeout:  3000,
		// The README cluster example targets plus ip:/domain: shapes.
		Targets: []string{
			"cidr:149.154.160.0/20",
			"cidr:91.108.0.0/16",
			"ip:8.8.8.8",
			"domain:telegram.org",
			"example.com:8443",
		},
		Peers: []PeerConfig{{Name: "la-1", Host: "la.example.com", Port: 443, Password: "s"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prober := NewProber(ctx, cfg, NewRouteTable("sg-1", 50), nil, NewClusterMetrics())

	if len(prober.staticTargets) != len(cfg.Targets) {
		t.Fatalf("expected %d probe targets, got %d: %+v",
			len(cfg.Targets), len(prober.staticTargets), prober.staticTargets)
	}

	// CIDR targets are keyed by the block and probed via a representative.
	cidr := prober.staticTargets[0]
	if cidr.Key() != "149.154.160.0/20" {
		t.Fatalf("expected CIDR key 149.154.160.0/20, got %q", cidr.Key())
	}
	if cidr.Host != "149.154.160.1" || cidr.Port != 443 {
		t.Fatalf("expected representative dial 149.154.160.1:443, got %s:%d", cidr.Host, cidr.Port)
	}

	// ip:/domain:/host:port shapes keep plain host:port keys.
	if key := prober.staticTargets[2].Key(); key != "8.8.8.8:443" {
		t.Fatalf("expected key 8.8.8.8:443, got %q", key)
	}
	if key := prober.staticTargets[3].Key(); key != "telegram.org:443" {
		t.Fatalf("expected key telegram.org:443, got %q", key)
	}
	if key := prober.staticTargets[4].Key(); key != "example.com:8443" {
		t.Fatalf("expected key example.com:8443, got %q", key)
	}
}

func TestRouteTableCIDRContainsLookup(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Prober-style write for a static CIDR target.
	rt.Update("149.154.160.0/20", "local", 300*time.Millisecond)
	rt.Update("149.154.160.0/20", "la-1", 15*time.Millisecond)

	// Data-path read for an IP inside the block: must hit the CIDR entry.
	peer, gain := rt.BestExit("149.154.175.53", 443)
	if peer != "la-1" {
		t.Fatalf("expected la-1 via CIDR containment, got %q", peer)
	}
	if gain != 285*time.Millisecond {
		t.Fatalf("expected gain 285ms, got %v", gain)
	}

	// An IP outside every configured block must not hit.
	if peer, _ := rt.BestExit("8.8.8.8", 443); peer != "" {
		t.Fatalf("expected no exit for unrelated IP, got %q", peer)
	}
}

func TestRouteTableCIDRMostSpecificWins(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("10.0.0.0/8", "local", 300*time.Millisecond)
	rt.Update("10.0.0.0/8", "peer-a", 10*time.Millisecond)
	rt.Update("10.1.0.0/16", "local", 300*time.Millisecond)
	rt.Update("10.1.0.0/16", "peer-b", 10*time.Millisecond)

	if peer, _ := rt.BestExit("10.1.2.3", 443); peer != "peer-b" {
		t.Fatalf("expected most specific CIDR to win (peer-b), got %q", peer)
	}
	if peer, _ := rt.BestExit("10.2.3.4", 443); peer != "peer-a" {
		t.Fatalf("expected /8 fallback (peer-a), got %q", peer)
	}
}

func TestRouteTableWriteReadKeyConsistency(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Write side: exactly what the prober / dynamic registration produce.
	rt.Update("telegram.org:443", "local", 249*time.Millisecond) // dynamic domain target
	rt.Update("telegram.org:443", "la-1", 15*time.Millisecond)
	rt.Update("149.154.160.0/20", "local", 249*time.Millisecond) // static CIDR target
	rt.Update("149.154.160.0/20", "la-1", 15*time.Millisecond)

	// Read side: the spellings lookupHosts produces for a dialed domain
	// (the domain itself, then its resolved IP) and for a dialed IP.
	cases := []struct {
		host string
		port int
	}{
		{"telegram.org", 443},  // domain spelling of the dynamic target
		{"149.154.175.53", 443}, // an IP inside the static CIDR
	}
	for _, c := range cases {
		peer, _ := rt.BestExit(c.host, c.port)
		if peer != "la-1" {
			t.Fatalf("lookup %s:%d expected la-1, got %q", c.host, c.port, peer)
		}
	}
}

func TestLookupHostsReusesAddrIPWithoutResolving(t *testing.T) {
	calls := swapResolver(t, func(host string) (string, error) {
		t.Fatalf("resolver must not be called when addr.IP is already set")
		return "", nil
	})

	addr := tunnel.NewAddressFromHostPort("tcp", "pre-resolved.test", 443)
	addr.IP = net.ParseIP("192.0.2.77") // what DialConn's write-back produces

	hosts := lookupHosts(addr)
	if len(hosts) != 2 || hosts[0] != "pre-resolved.test" || hosts[1] != "192.0.2.77" {
		t.Fatalf("unexpected lookup hosts %v", hosts)
	}
	if n := calls.load(); n != 0 {
		t.Fatalf("expected 0 resolver calls, got %d", n)
	}
}

func TestRouterDialConnWritesBackResolvedIP(t *testing.T) {
	// Issue #4: DialConn resolves the domain once and writes the IP back
	// onto the shared address so the downstream freedom dial reuses it.
	swapResolver(t, func(host string) (string, error) {
		if host == "dial-writeback.test" {
			return "192.0.2.55", nil
		}
		return "", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:        true,
		NodeName:       "sg-1",
		ProbeInterval:  120,
		ProbeTimeout:   3000,
		RelayThreshold: 50,
		Peers:          []PeerConfig{{Name: "la-1", Host: "la.example.com", Port: 443, Password: "s"}},
	}
	rt := NewRouteTable("sg-1", 50) // empty table -> local decision
	cr := &ClusterRouter{
		enabled:     true,
		routeTable:  rt,
		prober:      NewProber(ctx, cfg, rt, nil, NewClusterMetrics()),
		peerDialers: map[string]*PeerDialer{},
		matcher:     NewTargetMatcher(nil),
		metrics:     NewClusterMetrics(),
		localName:   "sg-1",
		cfg:         cfg,
	}

	addr := tunnel.NewAddressFromHostPort("tcp", "dial-writeback.test", 443)
	conn, node, err := cr.DialConn(addr)
	if err != nil || conn != nil || node != "local" {
		t.Fatalf("expected local decision, got conn=%v node=%q err=%v", conn, node, err)
	}
	if addr.AddressType != tunnel.DomainName {
		t.Fatalf("write-back must keep the domain address type for relay headers, got %v", addr.AddressType)
	}
	if addr.IP == nil || addr.IP.String() != "192.0.2.55" {
		t.Fatalf("expected resolved IP written back onto the address, got %v", addr.IP)
	}
}

// --- End-to-end shape for issue #5 "Done means" ---
// With the README cluster example configuration (two Telegram CIDR blocks,
// one peer), probe results published by the prober under CIDR keys must
// surface in /api/cluster optimized_routes.

func TestSnapshotShowsCIDROptimizedRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:          true,
		NodeName:         "singapore-1",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		RelayThreshold:   50,
		LatencyThreshold: 100,
		Targets:          []string{"cidr:149.154.160.0/20", "cidr:91.108.0.0/16"},
		Peers:            []PeerConfig{{Name: "la-1", Host: "la.example.com", Port: 443, Password: "s"}},
	}

	rt := NewRouteTable("singapore-1", 50)
	metrics := NewClusterMetrics()
	pd, err := NewPeerDialer(ctx, cfg.Peers[0])
	if err != nil {
		t.Fatalf("NewPeerDialer: %v", err)
	}
	defer pd.Close()
	peerDialers := map[string]*PeerDialer{"la-1": pd}
	prober := NewProber(ctx, cfg, rt, peerDialers, metrics)

	// The README example probes must actually exist now.
	found := map[string]bool{}
	for _, st := range prober.staticTargets {
		found[st.Key()] = true
	}
	for _, key := range []string{"149.154.160.0/20", "91.108.0.0/16"} {
		if !found[key] {
			t.Fatalf("prober has no static target for %s: %+v", key, prober.staticTargets)
		}
	}

	// Simulate one probe cycle: local is slow toward both blocks, the LA
	// peer is fast (the README scenario: 249ms vs 15ms).
	for _, key := range []string{"149.154.160.0/20", "91.108.0.0/16"} {
		rt.Update(key, "singapore-1", 249*time.Millisecond)
		rt.Update(key, "la-1", 15*time.Millisecond)
	}

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
	routes := map[string]OptimizedRouteEntry{}
	for _, r := range snap.OptimizedRoutes {
		routes[r.Target] = r
	}
	for _, key := range []string{"149.154.160.0/20", "91.108.0.0/16"} {
		r, ok := routes[key]
		if !ok {
			t.Fatalf("optimized_routes missing %s; got %+v", key, snap.OptimizedRoutes)
		}
		if r.BestPeer != "la-1" {
			t.Fatalf("expected best peer la-1 for %s, got %q", key, r.BestPeer)
		}
		if r.GainMs <= 0 {
			t.Fatalf("expected positive gain for %s, got %f", key, r.GainMs)
		}
	}

	// And the decision path must agree: traffic to an IP inside either
	// block is routed through la-1.
	if peer, _ := rt.BestExit("149.154.175.53", 443); peer != "la-1" {
		t.Fatalf("expected la-1 for in-block IP, got %q", peer)
	}
}

// --- Issue #12: weight breaks RTT ties ---

func TestRouteTableWeightTieBreak(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.SetPeerWeights(map[string]int{"peer-a": 0, "peer-b": 10})

	// Identical RTTs for both peers: without weights the first stored
	// record would win; with weights peer-b must win regardless of order.
	rt.Update("tie.test:443", "local", 200*time.Millisecond)
	rt.Update("tie.test:443", "peer-a", 20*time.Millisecond)
	rt.Update("tie.test:443", "peer-b", 20*time.Millisecond)

	for i := 0; i < 50; i++ {
		peer, _ := rt.BestExit("tie.test", 443)
		if peer != "peer-b" {
			t.Fatalf("iter %d: expected weight tie-break to pick peer-b, got %q", i, peer)
		}
		if peer := rt.FastestPeer("tie.test", 443); peer != "peer-b" {
			t.Fatalf("iter %d: FastestPeer expected peer-b, got %q", i, peer)
		}
	}

	// Weight must not override an actual RTT difference.
	rt.Update("tie.test:443", "peer-a", 10*time.Millisecond)
	if peer, _ := rt.BestExit("tie.test", 443); peer != "peer-a" {
		t.Fatalf("lower RTT must beat higher weight, got %q", peer)
	}
}
