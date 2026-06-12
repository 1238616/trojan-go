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
	return conn, bestPeer, nil
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

	resp := ClusterAPIResponse{
		Enabled:          cr.enabled,
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
