package cluster

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// trackedConn wraps a relay connection to track active connection count.
type trackedConn struct {
	net.Conn
	peerName string
	metrics  *ClusterMetrics
	once     sync.Once
}

func (tc *trackedConn) Close() error {
	tc.once.Do(func() {
		tc.metrics.RecordRelayClose(tc.peerName)
	})
	return tc.Conn.Close()
}

// ClusterRouter is the decision engine that intercepts outbound connections
// and routes them through optimal peers when beneficial.
type ClusterRouter struct {
	mu          sync.RWMutex
	enabled     bool
	routeTable  *RouteTable
	prober      *Prober
	peerDialers map[string]*PeerDialer
	matcher     *TargetMatcher
	metrics     *ClusterMetrics
	localName   string
	cfg         *Config
}

// globalRouter is the singleton accessed by the HTTP API.
var (
	globalRouter     *ClusterRouter
	globalRouterOnce sync.Once
)

// GlobalRouter returns the singleton ClusterRouter (may be nil if not enabled).
func GlobalRouter() *ClusterRouter {
	return globalRouter
}

// NewClusterRouter creates and initializes the cluster routing system.
func NewClusterRouter(ctx context.Context, cfg *Config) (*ClusterRouter, error) {
	if !cfg.Enabled || len(cfg.Peers) == 0 {
		return nil, nil
	}

	metrics := NewClusterMetrics()
	matcher := NewTargetMatcher(cfg.Targets)
	routeTable := NewRouteTable(cfg.NodeName, cfg.RelayThreshold)

	// Create peer dialers
	peerDialers := make(map[string]*PeerDialer, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		dialer, err := NewPeerDialer(ctx, peer)
		if err != nil {
			log.Warnf("cluster: failed to create peer dialer for %s: %v", peer.Name, err)
			continue
		}
		peerDialers[peer.Name] = dialer
	}

	if len(peerDialers) == 0 {
		return nil, fmt.Errorf("cluster: no valid peer dialers created")
	}

	// Create prober
	prober := NewProber(ctx, cfg, routeTable, peerDialers, metrics)

	cr := &ClusterRouter{
		enabled:     true,
		routeTable:  routeTable,
		prober:      prober,
		peerDialers: peerDialers,
		matcher:     matcher,
		metrics:     metrics,
		localName:   cfg.NodeName,
		cfg:         cfg,
	}

	// Set singleton
	globalRouterOnce.Do(func() {
		globalRouter = cr
	})

	return cr, nil
}

// Start launches background probing.
func (cr *ClusterRouter) Start() {
	if cr == nil || !cr.enabled {
		return
	}
	cr.prober.Start()
	log.Info("cluster: router started")
}

// Stop halts probing and closes peer connections.
func (cr *ClusterRouter) Stop() {
	if cr == nil {
		return
	}
	cr.prober.Stop()
	for _, d := range cr.peerDialers {
		d.Close()
	}
	log.Info("cluster: router stopped")
}

// DialConn is the core decision function.
// Returns (conn, peerName, nil) if relay is beneficial.
// Returns (nil, "local", nil) if local direct is optimal.
// The caller should fall back to direct dial when conn is nil.
func (cr *ClusterRouter) DialConn(addr *tunnel.Address) (net.Conn, string, error) {
	if cr == nil || !cr.enabled {
		return nil, "local", nil
	}

	// ForceRelay: route ALL outbound through the fastest available peer,
	// skipping matcher and local RTT comparison entirely.
	if cr.cfg != nil && cr.cfg.ForceRelay {
		return cr.dialForceRelay(addr)
	}

	// Check if target is in cluster-managed range.
	// When no static targets are configured, we rely on dynamic target
	// registration (RegisterSlowTarget) — skip matcher and go straight
	// to route table lookup.
	hasStaticRules := cr.matcher.HasRules()
	if hasStaticRules {
		matched := cr.matcher.Match(addr)
		if cr.cfg != nil {
			log.DebugKV("cluster: route check (static matcher)",
				"target", addr.String(),
				"matched", matched)
		}
		if !matched {
			return nil, "local", nil
		}
	}

	// Resolve IP for route table lookup
	targetIP := ""
	if addr.IP != nil {
		targetIP = addr.IP.String()
	} else if addr.DomainName != "" {
		resolved, err := net.ResolveIPAddr("ip", addr.DomainName)
		if err == nil {
			targetIP = resolved.IP.String()
		}
	}
	if targetIP == "" {
		return nil, "local", nil
	}

	// Query route table
	bestPeer, gain := cr.routeTable.BestExit(targetIP)
	log.DebugKV("cluster: route table query",
		"target_ip", targetIP,
		"best_peer", bestPeer,
		"gain_ms", gain.Milliseconds(),
		"has_static_rules", hasStaticRules)
	if bestPeer == "" {
		return nil, "local", nil
	}

	// Relay through best peer
	dialer, ok := cr.peerDialers[bestPeer]
	if !ok {
		return nil, "local", nil
	}

	log.InfoKV("cluster: relay decision",
		"target", addr.String(),
		"peer", bestPeer,
		"gain_ms", gain.Milliseconds())

	conn, err := dialer.DialConn(addr)
	if err != nil {
		log.WarnKV("cluster: relay failed, fallback to local",
			"peer", bestPeer, "target", addr.String(), "err", err)
		cr.metrics.RecordRelayFallback(bestPeer)
		return nil, "local", nil
	}

	cr.metrics.RecordRelay(bestPeer, gain)
	cr.metrics.RecordRelayOpen(bestPeer)
	return &trackedConn{Conn: conn, peerName: bestPeer, metrics: cr.metrics}, bestPeer, nil
}

// dialForceRelay handles the ForceRelay path: pick the fastest peer from
// route table data, or fall back to any available peer if no probe data exists.
func (cr *ClusterRouter) dialForceRelay(addr *tunnel.Address) (net.Conn, string, error) {
	var selectedPeer string

	// Try route-table lookup for best peer with probe data
	targetIP := ""
	if addr.IP != nil {
		targetIP = addr.IP.String()
	} else if addr.DomainName != "" {
		resolved, err := net.ResolveIPAddr("ip", addr.DomainName)
		if err == nil {
			targetIP = resolved.IP.String()
		}
	}
	if targetIP != "" {
		selectedPeer = cr.routeTable.FastestPeer(targetIP)
	}

	// No probe data for this target — pick first available peer
	if selectedPeer == "" {
		for name := range cr.peerDialers {
			selectedPeer = name
			break
		}
	}
	if selectedPeer == "" {
		return nil, "local", nil
	}

	dialer, ok := cr.peerDialers[selectedPeer]
	if !ok {
		return nil, "local", nil
	}

	log.InfoKV("cluster: force-relay",
		"target", addr.String(),
		"peer", selectedPeer)

	conn, err := dialer.DialConn(addr)
	if err != nil {
		log.WarnKV("cluster: force-relay failed, fallback to local",
			"peer", selectedPeer, "target", addr.String(), "err", err)
		cr.metrics.RecordRelayFallback(selectedPeer)
		return nil, "local", nil
	}

	cr.metrics.RecordRelay(selectedPeer, 0)
	cr.metrics.RecordRelayOpen(selectedPeer)
	return &trackedConn{Conn: conn, peerName: selectedPeer, metrics: cr.metrics}, selectedPeer, nil
}

// DialAnyPeer is the emergency fallback used when local freedom dial has
// already failed for the current connection. It tries to relay through any
// peer that has a chance of reaching the target, so the in-flight user
// connection isn't dropped while waiting for the next probe cycle to
// repopulate the route table.
//
// Candidate order:
//  1. The route-table BestExit peer for this target IP (if probe data exists
//     and shows a reachable peer).
//  2. Every other peer that the prober currently considers alive.
//
// Returns (conn, peerName, nil) on the first successful peer dial. Returns
// (nil, "", err) when no peer succeeds.
func (cr *ClusterRouter) DialAnyPeer(addr *tunnel.Address) (net.Conn, string, error) {
	if cr == nil || !cr.enabled {
		return nil, "", fmt.Errorf("cluster router not enabled")
	}

	tried := make(map[string]struct{})
	candidates := make([]string, 0, len(cr.peerDialers)+1)

	// Candidate 1: route-table best exit (if any).
	targetIP := ""
	if addr.IP != nil {
		targetIP = addr.IP.String()
	} else if addr.DomainName != "" {
		if resolved, err := net.ResolveIPAddr("ip", addr.DomainName); err == nil {
			targetIP = resolved.IP.String()
		}
	}
	if targetIP != "" {
		if best, _ := cr.routeTable.BestExit(targetIP); best != "" {
			if _, ok := cr.peerDialers[best]; ok {
				candidates = append(candidates, best)
				tried[best] = struct{}{}
			}
		}
	}

	// Candidate 2..N: alive peers (deduped).
	for name := range cr.peerDialers {
		if _, seen := tried[name]; seen {
			continue
		}
		if cr.prober != nil && !cr.prober.IsPeerAlive(name) {
			continue
		}
		candidates = append(candidates, name)
		tried[name] = struct{}{}
	}

	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("no alive peer available for fallback")
	}

	var lastErr error
	for _, name := range candidates {
		dialer, ok := cr.peerDialers[name]
		if !ok {
			continue
		}
		conn, err := dialer.DialConn(addr)
		if err != nil {
			cr.metrics.RecordRelayFallback(name)
			log.DebugKV("cluster: emergency fallback peer dial failed",
				"peer", name, "target", addr.String(), "err", err)
			lastErr = err
			continue
		}
		log.InfoKV("cluster: emergency fallback via peer",
			"peer", name, "target", addr.String())
		cr.metrics.RecordRelay(name, 0)
		cr.metrics.RecordRelayOpen(name)
		return &trackedConn{Conn: conn, peerName: name, metrics: cr.metrics}, name, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no peer dialer available")
	}
	return nil, "", lastErr
}

// RegisterSlowTarget exposes the prober's dynamic target registration
// for use in the data path (freedom.Client.DialConn).
func (cr *ClusterRouter) RegisterSlowTarget(host string, port int, dialRTT time.Duration) {
	if cr == nil || !cr.enabled {
		return
	}
	cr.prober.RegisterSlowTarget(host, port, dialRTT)
}

// Snapshot returns the current cluster state for the HTTP API.
func (cr *ClusterRouter) Snapshot() ClusterAPIResponse {
	if cr == nil {
		return ClusterAPIResponse{Enabled: false}
	}

	mode := "optimized"
	if cr.cfg.ForceRelay {
		mode = "force_relay"
	}

	resp := ClusterAPIResponse{
		Enabled:          cr.enabled,
		ForceRelay:       cr.cfg.ForceRelay,
		Mode:             mode,
		LocalNode:        cr.localName,
		ProbeInterval:    cr.cfg.ProbeInterval,
		LatencyThreshold: cr.cfg.LatencyThreshold,
	}

	// Peer status
	entries := cr.routeTable.GetAllEntries()
	for _, peer := range cr.cfg.Peers {
		ps := PeerStatus{
			Name: peer.Name,
			Host: peer.Host,
			Port: peer.Port,
		}

		// Per-peer metrics
		peerMetrics := cr.metrics.PeerSnapshot(peer.Name)
		ps.ActiveRelays = peerMetrics.ActiveConns
		ps.TotalRelays = peerMetrics.TotalRelays
		ps.TotalFallbacks = peerMetrics.TotalFallbacks

		// Connection latency from last alive check
		if dialer, ok := cr.peerDialers[peer.Name]; ok {
			rtt := dialer.CheckAlive(3 * time.Second)
			if rtt >= 0 {
				ps.ConnLatencyMs = float64(rtt.Microseconds()) / 1000.0
			} else {
				ps.ConnLatencyMs = -1
			}
		}

		available := false
		for _, peers := range entries {
			for _, pl := range peers {
				if pl.PeerName == peer.Name {
					if pl.Available {
						available = true
					}
					ps.TargetRTTs = append(ps.TargetRTTs, TargetRTT{
						Target:    "", // filled below
						RTTMs:     float64(pl.RTT.Microseconds()) / 1000.0,
						RawRTTMs:  float64(pl.RawRTT.Microseconds()) / 1000.0,
						Available: pl.Available,
					})
					if ps.LastProbeAt == "" || pl.UpdatedAt.Format(time.RFC3339) > ps.LastProbeAt {
						ps.LastProbeAt = pl.UpdatedAt.Format(time.RFC3339)
					}
				}
			}
		}
		ps.Available = available
		if !available && cr.prober != nil {
			ps.Available = cr.prober.IsPeerAlive(peer.Name)
		}

		// Fill target keys into TargetRTTs
		i := 0
		for targetKey, peers := range entries {
			for _, pl := range peers {
				if pl.PeerName == peer.Name && i < len(ps.TargetRTTs) {
					ps.TargetRTTs[i].Target = targetKey
					i++
				}
			}
		}

		resp.Peers = append(resp.Peers, ps)
	}

	// Optimized routes
	for targetKey, peers := range entries {
		var localRTT time.Duration
		localFound := false
		localAvailable := false
		var bestPeer PeerLatency
		for _, pl := range peers {
			if pl.PeerName == cr.localName {
				localRTT = pl.RTT
				localFound = true
				localAvailable = pl.Available
			} else if pl.Available && (bestPeer.PeerName == "" || pl.RTT < bestPeer.RTT) {
				bestPeer = pl
			}
		}
		if bestPeer.PeerName == "" {
			continue
		}

		var gain time.Duration
		var gainPct float64

		var displayLocalRTTMs float64
		if localFound && !localAvailable {
			// Local unreachable, peer can reach: primary cluster routing case
			gain = unreachableGain
			gainPct = 100.0
			displayLocalRTTMs = -1
		} else if localRTT > 0 && localRTT-bestPeer.RTT > cr.routeTable.relayThreshold {
			// Local reachable but peer is meaningfully faster
			gain = localRTT - bestPeer.RTT
			gainPct = float64(gain) / float64(localRTT) * 100
			displayLocalRTTMs = float64(localRTT.Microseconds()) / 1000.0
		} else {
			continue
		}

		source := "static"
		firstSeen := ""
		if v, ok := cr.prober.dynamicTargets.Load(targetKey); ok {
			source = "dynamic"
			firstSeen = v.(*DynamicTarget).FirstSeenAt.Format(time.RFC3339)
		}

		resp.OptimizedRoutes = append(resp.OptimizedRoutes, OptimizedRouteEntry{
			Target:      targetKey,
			LocalRTTMs:  displayLocalRTTMs,
			BestPeer:    bestPeer.PeerName,
			PeerRTTMs:   float64(bestPeer.RTT.Microseconds()) / 1000.0,
			GainMs:      float64(gain.Microseconds()) / 1000.0,
			GainPercent: gainPct,
			Source:      source,
			FirstSeenAt: firstSeen,
		})
	}

	// Stats
	stats := cr.metrics.Snapshot()
	stats.StaticTargets = len(cr.prober.staticTargets)
	stats.DynamicTargets = cr.prober.DynamicTargetCount()
	stats.ProbeTargets = stats.StaticTargets + stats.DynamicTargets
	resp.Stats = stats

	return resp
}
