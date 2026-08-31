package cluster

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// addrKey builds the "host:port" identity used for the emergency-fallback
// negative cache, preferring the dialed domain name.
func addrKey(addr *tunnel.Address) string {
	host := addr.DomainName
	if host == "" && addr.IP != nil {
		host = addr.IP.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(addr.Port))
}

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

// fallbackNegCacheTTL is how long DialAnyPeer remembers that a target
// failed through every candidate peer. Within the TTL further emergency
// fallbacks for the same target short-circuit, so a hot target that is
// down everywhere doesn't amplify each user connection into N full peer
// handshakes.
const fallbackNegCacheTTL = 10 * time.Second

// ClusterRouter is the decision engine that intercepts outbound connections
// and routes them through optimal peers when beneficial.
type ClusterRouter struct {
	enabled     bool
	routeTable  *RouteTable
	prober      *Prober
	peerDialers map[string]*PeerDialer
	matcher     *TargetMatcher
	metrics     *ClusterMetrics
	localName   string
	cfg         *Config

	// fallbackNegCache maps "host:port" to the time until which an
	// emergency DialAnyPeer for that target should be skipped because the
	// previous attempt already failed through every alive peer.
	fallbackNegCache sync.Map
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

	// Wire peer weights into route selection: they break exact RTT ties
	// in BestExit / FastestPeer (issue #12).
	weights := make(map[string]int, len(cfg.Peers))
	for _, peer := range cfg.Peers {
		if peer.Weight != 0 {
			weights[peer.Name] = peer.Weight
		}
	}
	if len(weights) > 0 {
		routeTable.SetPeerWeights(weights)
	}

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

// lookupHosts returns the ordered route-table lookup keys for an address:
// the dialed host first, then its resolved IP. Dynamic targets are
// registered under the exact host the client dialed (usually a domain),
// while static probe targets are IPs, so both spellings must be tried for
// either kind to match. Resolution is cached (see dns_cache.go), keeping
// the hot path to at most one cached lookup.
func lookupHosts(addr *tunnel.Address) []string {
	if addr.DomainName != "" {
		hosts := []string{addr.DomainName}
		ip := addr.IP
		if ip == nil {
			if s := resolveTargetIP(addr.DomainName); s != "" {
				ip = net.ParseIP(s)
			}
		}
		if ip != nil {
			hosts = append(hosts, ip.String())
		}
		return hosts
	}
	if addr.IP != nil {
		return []string{addr.IP.String()}
	}
	return nil
}

// bestExitForAddr queries the route table for the best relay peer.
func (cr *ClusterRouter) bestExitForAddr(addr *tunnel.Address) (string, time.Duration) {
	for _, host := range lookupHosts(addr) {
		if peer, gain := cr.routeTable.BestExit(host, addr.Port); peer != "" {
			return peer, gain
		}
	}
	return "", 0
}

// fastestPeerForAddr queries the route table for the fastest peer
// (ForceRelay mode).
func (cr *ClusterRouter) fastestPeerForAddr(addr *tunnel.Address) string {
	for _, host := range lookupHosts(addr) {
		if peer := cr.routeTable.FastestPeer(host, addr.Port); peer != "" {
			return peer
		}
	}
	return ""
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

	// Resolve a domain target once, before any decision layer runs, and
	// write the result back onto the address (issue #4). Everything
	// downstream — matcher, route-table lookup, emergency fallback and the
	// local freedom dial — reuses this resolution instead of re-resolving:
	// the shared 30s cache plus this write-back keep the per-connection
	// resolver cost at most one lookup, and the trojan header still carries
	// the original domain because AddressType is left untouched.
	if addr.AddressType == tunnel.DomainName && addr.IP == nil && addr.DomainName != "" {
		if ipStr := resolveTargetIP(addr.DomainName); ipStr != "" {
			addr.IP = net.ParseIP(ipStr)
		}
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

	// Query route table. lookupHosts returns the dialed host plus (if it's
	// a domain) its resolved IP, so both static-IP and dynamic-domain
	// probe entries can match.
	bestPeer, gain := cr.bestExitForAddr(addr)
	log.DebugKV("cluster: route table query",
		"target", addr.String(),
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
	selectedPeer = cr.fastestPeerForAddr(addr)

	// No probe data for this target — pick deterministically: highest
	// configured weight first, then lexicographic name, instead of random
	// map iteration order.
	if selectedPeer == "" {
		names := make([]string, 0, len(cr.peerDialers))
		for name := range cr.peerDialers {
			names = append(names, name)
		}
		sort.Strings(names)
		bestWeight := -1
		for _, name := range names {
			if w := cr.routeTable.weight(name); w > bestWeight {
				selectedPeer = name
				bestWeight = w
			}
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

	// Negative cache: if a previous emergency fallback for this exact
	// target already failed through every candidate within the TTL, fail
	// fast instead of repeating N full peer handshakes per user
	// connection while the target is down everywhere.
	key := addrKey(addr)
	if v, ok := cr.fallbackNegCache.Load(key); ok {
		if expiry, isTime := v.(time.Time); isTime {
			if time.Now().Before(expiry) {
				return nil, "", fmt.Errorf("cluster: fallback for %s suppressed (all peers failed recently)", key)
			}
			cr.fallbackNegCache.Delete(key)
		}
	}

	tried := make(map[string]struct{})
	candidates := make([]string, 0, len(cr.peerDialers)+1)

	// Candidate 1: route-table best exit (if any).
	if best, _ := cr.bestExitForAddr(addr); best != "" {
		if _, ok := cr.peerDialers[best]; ok {
			candidates = append(candidates, best)
			tried[best] = struct{}{}
		}
	}

	// Candidate 2..N: alive peers (deduped, sorted for determinism).
	peerNames := make([]string, 0, len(cr.peerDialers))
	for name := range cr.peerDialers {
		if _, seen := tried[name]; seen {
			continue
		}
		if cr.prober != nil && !cr.prober.IsPeerAlive(name) {
			continue
		}
		peerNames = append(peerNames, name)
	}
	sort.Strings(peerNames)
	for _, name := range peerNames {
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
		cr.fallbackNegCache.Delete(key) // target reachable again — clear any stale negative
		return &trackedConn{Conn: conn, peerName: name, metrics: cr.metrics}, name, nil
	}

	// Every candidate failed — suppress retries for this target briefly.
	cr.fallbackNegCache.Store(key, time.Now().Add(fallbackNegCacheTTL))

	if lastErr == nil {
		lastErr = fmt.Errorf("no peer dialer available")
	}
	return nil, "", lastErr
}

// RegisterSlowTarget exposes the prober's dynamic target registration
// for use in the data path (freedom.Client.DialConn). dialFailed marks
// the local node unreachable for the target when the dial actually failed
// (timeout); a slow-but-successful dial must pass false.
func (cr *ClusterRouter) RegisterSlowTarget(host string, port int, dialRTT time.Duration, dialFailed bool) {
	if cr == nil || !cr.enabled {
		return
	}
	cr.prober.RegisterSlowTarget(host, port, dialRTT, dialFailed)
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
	// Sort target keys so the API output — and the per-peer RTT lists
	// built below — are deterministic instead of following random map
	// iteration order.
	targetKeys := make([]string, 0, len(entries))
	for k := range entries {
		targetKeys = append(targetKeys, k)
	}
	sort.Strings(targetKeys)

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

		// Connection latency from the prober's cached alive check. The
		// prober re-measures every cycle; serving the cache here keeps
		// /api/cluster from running a serial full-stack handshake per
		// peer per request. -1 until the first probe lands.
		ps.ConnLatencyMs = -1
		if cr.prober != nil {
			if info, ok := cr.prober.PeerAliveInfo(peer.Name); ok {
				if info.Alive {
					ps.ConnLatencyMs = float64(info.RTT.Microseconds()) / 1000.0
				}
			}
		}

		available := false
		// Single pass over the entries fills target name and RTT together
		// so they can never end up misaligned.
		for _, targetKey := range targetKeys {
			for _, pl := range entries[targetKey] {
				if pl.PeerName != peer.Name {
					continue
				}
				if pl.Available {
					available = true
				}
				ps.TargetRTTs = append(ps.TargetRTTs, TargetRTT{
					Target:    targetKey,
					RTTMs:     float64(pl.RTT.Microseconds()) / 1000.0,
					RawRTTMs:  float64(pl.RawRTT.Microseconds()) / 1000.0,
					Available: pl.Available,
				})
				if ps.LastProbeAt == "" || pl.UpdatedAt.Format(time.RFC3339) > ps.LastProbeAt {
					ps.LastProbeAt = pl.UpdatedAt.Format(time.RFC3339)
				}
			}
		}
		ps.Available = available
		if !available && cr.prober != nil {
			ps.Available = cr.prober.IsPeerAlive(peer.Name)
		}

		resp.Peers = append(resp.Peers, ps)
	}

	// Optimized routes
	for _, targetKey := range targetKeys {
		peers := entries[targetKey]
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
		if cr.prober != nil {
			if v, ok := cr.prober.dynamicTargets.Load(targetKey); ok {
				source = "dynamic"
				firstSeen = v.(*DynamicTarget).FirstSeenAt.Format(time.RFC3339)
			}
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
	if cr.prober != nil {
		stats.StaticTargets = len(cr.prober.staticTargets)
		stats.DynamicTargets = cr.prober.DynamicTargetCount()
		stats.ProbeTargets = stats.StaticTargets + stats.DynamicTargets
	}
	resp.Stats = stats

	return resp
}
