package cluster

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestProberIntervalClamp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:          true,
		NodeName:         "local",
		ProbeInterval:    30, // below minimum 60
		ProbeTimeout:     1000,
		LatencyThreshold: 100,
	}

	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	if p.interval != 60*time.Second {
		t.Fatalf("expected interval clamped to 60s, got %v", p.interval)
	}
}

func TestProberRegisterSlowTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		Enabled:          true,
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 100, // 100ms threshold
	}

	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	// Above threshold → should register
	p.RegisterSlowTarget("149.154.175.53", 443, 250*time.Millisecond, false)

	if p.DynamicTargetCount() != 1 {
		t.Fatalf("expected 1 dynamic target, got %d", p.DynamicTargetCount())
	}

	// Verify stored correctly
	key := "149.154.175.53:443"
	v, ok := p.dynamicTargets.Load(key)
	if !ok {
		t.Fatal("expected target to be stored")
	}
	dt := v.(*DynamicTarget)
	if dt.Host != "149.154.175.53" {
		t.Fatalf("expected host=149.154.175.53, got %q", dt.Host)
	}
	if dt.Port != 443 {
		t.Fatalf("expected port=443, got %d", dt.Port)
	}
}

func TestProberRegisterBelowThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 100,
	}

	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	// Below threshold → should NOT register
	p.RegisterSlowTarget("8.8.8.8", 53, 50*time.Millisecond, false)

	if p.DynamicTargetCount() != 0 {
		t.Fatalf("expected 0 dynamic targets, got %d", p.DynamicTargetCount())
	}
}

func TestProberEvictStaleTargets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 100,
	}

	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	// Register two targets through the normal path (keeps the atomic
	// counter consistent), then age one past the 30-minute cutoff by
	// publishing an immutable copy with an old LastSeenAt.
	p.RegisterSlowTarget("old", 443, 200*time.Millisecond, false)
	p.RegisterSlowTarget("fresh", 443, 200*time.Millisecond, false)

	v, ok := p.dynamicTargets.Load("old:443")
	if !ok {
		t.Fatal("expected old target to be registered")
	}
	aged := *v.(*DynamicTarget)
	aged.LastSeenAt = time.Now().Add(-31 * time.Minute)
	p.dynamicTargets.Store("old:443", &aged)

	if p.DynamicTargetCount() != 2 {
		t.Fatalf("expected 2 before eviction, got %d", p.DynamicTargetCount())
	}

	p.evictStaleTargets()

	if p.DynamicTargetCount() != 1 {
		t.Fatalf("expected 1 after eviction, got %d", p.DynamicTargetCount())
	}

	if _, ok := p.dynamicTargets.Load("old:443"); ok {
		t.Fatal("expected old target to be evicted")
	}
	if _, ok := p.dynamicTargets.Load("fresh:443"); !ok {
		t.Fatal("expected fresh target to remain")
	}
}

func TestProberDynamicTargetRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:         "local",
		ProbeInterval:    120,
		ProbeTimeout:     3000,
		LatencyThreshold: 100,
	}

	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	// Register
	firstSeen := time.Now()
	p.RegisterSlowTarget("1.2.3.4", 443, 200*time.Millisecond, false)
	time.Sleep(10 * time.Millisecond)

	// Re-register with new RTT → should update
	p.RegisterSlowTarget("1.2.3.4", 443, 300*time.Millisecond, false)

	v, _ := p.dynamicTargets.Load("1.2.3.4:443")
	dt := v.(*DynamicTarget)
	if dt.LastDialRTT != 300*time.Millisecond {
		t.Fatalf("expected refreshed RTT=300ms, got %v", dt.LastDialRTT)
	}
	// FirstSeenAt must keep the original registration time (it is
	// "first seen", not "last seen").
	if dt.FirstSeenAt.Before(firstSeen.Add(-time.Second)) || dt.FirstSeenAt.After(firstSeen.Add(time.Second)) {
		t.Fatalf("expected FirstSeenAt preserved around %v, got %v", firstSeen, dt.FirstSeenAt)
	}
	if dt.LastSeenAt.Before(dt.FirstSeenAt) {
		t.Fatalf("expected LastSeenAt >= FirstSeenAt, got %v < %v", dt.LastSeenAt, dt.FirstSeenAt)
	}
}

func TestProbeDirectTCP(t *testing.T) {
	// Start a local TCP listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:      "local",
		ProbeInterval: 120,
		ProbeTimeout:  3000,
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	rtt := p.probeDirect(ProbeTarget{Host: "127.0.0.1", Port: addr.Port})
	if rtt < 0 {
		t.Fatal("expected positive RTT for local listener")
	}
	if rtt > 100*time.Millisecond {
		t.Fatalf("local connection should be fast, got %v", rtt)
	}
}

func TestProbeDirectTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:      "local",
		ProbeInterval: 120,
		ProbeTimeout:  500, // 500ms timeout for faster test
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	// Use a port that's very unlikely to be listening
	rtt := p.probeDirect(ProbeTarget{Host: "192.0.2.1", Port: 1})
	if rtt >= 0 {
		t.Fatalf("expected -1 for unreachable target, got %v", rtt)
	}
}

func TestProberStaticTargetParsing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:      "local",
		ProbeInterval: 120,
		ProbeTimeout:  3000,
		Targets: []string{
			"149.154.175.53",      // bare IP → port 443
			"cidr:91.108.0.0/16",  // CIDR → skipped for static probe
			"domain:telegram.org", // domain → skipped
		},
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	// Only bare IPs become static probe targets
	if len(p.staticTargets) != 1 {
		t.Fatalf("expected 1 static target, got %d: %v", len(p.staticTargets), p.staticTargets)
	}
	if p.staticTargets[0].Host != "149.154.175.53" {
		t.Fatalf("expected host=149.154.175.53, got %q", p.staticTargets[0].Host)
	}
	if p.staticTargets[0].Port != 443 {
		t.Fatalf("expected port=443, got %d", p.staticTargets[0].Port)
	}
}

func TestProberHostPortTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &Config{
		NodeName:      "local",
		ProbeInterval: 120,
		ProbeTimeout:  3000,
		Targets: []string{
			fmt.Sprintf("127.0.0.1:%d", 8080),
		},
	}
	rt := NewRouteTable("local", 50)
	metrics := NewClusterMetrics()
	p := NewProber(ctx, cfg, rt, make(map[string]*PeerDialer), metrics)

	if len(p.staticTargets) != 1 {
		t.Fatalf("expected 1 static target, got %d", len(p.staticTargets))
	}
	if p.staticTargets[0].Port != 8080 {
		t.Fatalf("expected port=8080, got %d", p.staticTargets[0].Port)
	}
}
