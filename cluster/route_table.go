package cluster

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ewmaAlpha      = 0.3
	staleThreshold = 10 * time.Minute
	// unreachableGain is the synthetic gain reported when local cannot
	// reach a target but a peer can. Represents "infinite improvement".
	unreachableGain = 10 * time.Second
)

// PeerLatency records a single peer's measured RTT to a target.
type PeerLatency struct {
	PeerName  string
	RTT       time.Duration // EWMA-smoothed RTT
	RawRTT    time.Duration // last raw measurement
	UpdatedAt time.Time
	Available bool
}

// RouteTable maintains target → sorted peer latency mappings.
// Updated by Prober, queried by ClusterRouter.
//
// Entry keys come in two shapes, both written by the prober and both
// understood by match():
//   - "host:port" — exact probe targets and dynamically registered targets
//   - "<cidr>" (e.g. "149.154.160.0/20") — static CIDR targets, probed via
//     a representative address; matched by IP containment (issue #5)
type RouteTable struct {
	mu             sync.RWMutex
	entries        map[string][]PeerLatency // key: "host:port" or "<cidr>"
	localName      string
	relayThreshold time.Duration

	// cidrNets mirrors the CIDR-shaped keys of entries so match() can test
	// IP containment without re-parsing every key on every lookup.
	cidrNets map[string]*net.IPNet

	// peerWeights breaks RTT ties in BestExit / FastestPeer: when two peers
	// measure the same latency, the one with the higher configured weight
	// wins (issue #12; "weight: 0" keeps pure RTT ordering).
	peerWeights map[string]int
}

func NewRouteTable(localName string, relayThresholdMs int) *RouteTable {
	return &RouteTable{
		entries:        make(map[string][]PeerLatency),
		cidrNets:       make(map[string]*net.IPNet),
		localName:      localName,
		relayThreshold: time.Duration(relayThresholdMs) * time.Millisecond,
	}
}

// SetPeerWeights installs the per-peer weights used as RTT tie-breakers.
func (rt *RouteTable) SetPeerWeights(weights map[string]int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.peerWeights = weights
}

// isCIDRKey reports whether key parses as a CIDR block and returns it.
func isCIDRKey(key string) (*net.IPNet, bool) {
	if !strings.Contains(key, "/") {
		return nil, false
	}
	_, ipnet, err := net.ParseCIDR(key)
	if err != nil {
		return nil, false
	}
	return ipnet, true
}

// Update records a probe result for a peer→target pair.
// Uses EWMA smoothing to dampen transient spikes.
func (rt *RouteTable) Update(targetKey string, peerName string, rtt time.Duration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	// Track CIDR-shaped keys for the containment fallback in match().
	// Normalize the key (e.g. "149.154.165.5/20" -> "149.154.160.0/20")
	// so entries and cidrNets always agree on the spelling.
	if ipnet, ok := isCIDRKey(targetKey); ok {
		targetKey = ipnet.String()
		rt.cidrNets[targetKey] = ipnet
	}

	peers := rt.entries[targetKey]
	found := false
	for i := range peers {
		if peers[i].PeerName == peerName {
			found = true
			if rtt < 0 {
				peers[i].Available = false
				peers[i].RTT = 0
				peers[i].UpdatedAt = time.Now()
			} else {
				old := peers[i].RTT
				if old == 0 || !peers[i].Available {
					// Fresh start or recovery from failure: use raw value
					peers[i].RTT = rtt
				} else {
					peers[i].RTT = time.Duration(
						ewmaAlpha*float64(rtt) + (1-ewmaAlpha)*float64(old),
					)
				}
				peers[i].RawRTT = rtt
				peers[i].Available = true
				peers[i].UpdatedAt = time.Now()
			}
			break
		}
	}
	if !found {
		avail := rtt >= 0
		smoothed := rtt
		if !avail {
			smoothed = 0
		}
		peers = append(peers, PeerLatency{
			PeerName:  peerName,
			RTT:       smoothed,
			RawRTT:    rtt,
			UpdatedAt: time.Now(),
			Available: avail,
		})
		rt.entries[targetKey] = peers
	}
}

// BestExit returns the best peer for a target host:port.
// Returns ("", 0) if local is already optimal or no data available.
// When local is unreachable but a peer can reach the target, it returns
// that peer — this is the core cluster routing use case (bypassing blocks).
func (rt *RouteTable) BestExit(targetHost string, targetPort int) (peerName string, relayGain time.Duration) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	peers := rt.match(targetHost, targetPort)
	if len(peers) == 0 {
		return "", 0
	}

	var localRTT time.Duration
	localFound := false
	localAvailable := false
	var bestPeer PeerLatency

	now := time.Now()
	for _, p := range peers {
		if now.Sub(p.UpdatedAt) > staleThreshold {
			continue
		}
		if p.PeerName == rt.localName {
			localFound = true
			localAvailable = p.Available
			localRTT = p.RTT
			continue
		}
		if p.Available && rt.better(p, bestPeer) {
			bestPeer = p
		}
	}

	if bestPeer.PeerName == "" {
		return "", 0
	}

	// Local is unreachable (probe failed) but peer can reach target:
	// this is the primary use case — route around blocked destinations.
	if localFound && !localAvailable {
		return bestPeer.PeerName, unreachableGain
	}

	// No local data yet — can't make a decision
	if localRTT == 0 {
		return "", 0
	}

	// Normal case: local reachable, check if peer is meaningfully faster
	gain := localRTT - bestPeer.RTT
	if gain > rt.relayThreshold {
		return bestPeer.PeerName, gain
	}
	return "", 0
}

// FastestPeer returns the fastest available peer for a target host:port,
// ignoring local node entirely. Used by ForceRelay mode.
func (rt *RouteTable) FastestPeer(targetHost string, targetPort int) string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	peers := rt.match(targetHost, targetPort)
	now := time.Now()
	var best PeerLatency

	for _, p := range peers {
		if now.Sub(p.UpdatedAt) > staleThreshold {
			continue
		}
		if p.PeerName == rt.localName {
			continue
		}
		if p.Available && rt.better(p, best) {
			best = p
		}
	}
	return best.PeerName
}

// better reports whether p is a better exit choice than best: lower RTT
// wins; on exact RTT ties the higher configured peer weight wins
// (issue #12, weight: 0 keeps pure RTT ordering).
func (rt *RouteTable) better(p, best PeerLatency) bool {
	if best.PeerName == "" {
		return true
	}
	if p.RTT != best.RTT {
		return p.RTT < best.RTT
	}
	return rt.weight(p.PeerName) > rt.weight(best.PeerName)
}

func (rt *RouteTable) weight(name string) int {
	if rt.peerWeights == nil {
		return 0
	}
	return rt.peerWeights[name]
}

// match finds entries for a target. Lookup order:
//  1. Exact "host:port" match — the write key used by the prober and by
//     dynamic target registration, so same-spell lookups hit directly.
//  2. Host-only fallback aggregating entries recorded for the same host on
//     other ports (deterministic: sorted keys, merge by recency).
//  3. CIDR containment fallback: when the looked-up host is an IP and a
//     CIDR-keyed entry covers it (issue #5). Static cidr: targets are
//     probed via a representative address and stored under the CIDR key,
//     so traffic to any IP inside the block can use the measurements.
//
// All paths are deterministic: fallbacks walk keys in sorted order and merge
// per-peer records by recency, so the result never depends on Go map
// iteration order (which is randomized per process).
func (rt *RouteTable) match(targetHost string, targetPort int) []PeerLatency {
	// Exact "host:port" match.
	if peers, ok := rt.entries[net.JoinHostPort(targetHost, strconv.Itoa(targetPort))]; ok && len(peers) > 0 {
		return peers
	}

	// Host-only fallback: collect every key for this host, in sorted order
	// so the merge below is stable regardless of map layout.
	var keys []string
	for key := range rt.entries {
		host, _, err := net.SplitHostPort(key)
		if err != nil {
			host = key
		}
		if host == targetHost {
			keys = append(keys, key)
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return rt.mergeKeys(keys)
	}

	// CIDR containment fallback: the lookup host must be an IP and some
	// CIDR-keyed entry must contain it. When several CIDRs cover the IP,
	// the most specific one (largest prefix) wins; equal prefix lengths
	// break toward the lexicographically smaller key so the choice is
	// reproducible.
	if ip := net.ParseIP(targetHost); ip != nil && len(rt.cidrNets) > 0 {
		var bestKey string
		var bestOnes int
		for key, ipnet := range rt.cidrNets {
			if !ipnet.Contains(ip) {
				continue
			}
			ones, _ := ipnet.Mask.Size()
			if bestKey == "" || ones > bestOnes || (ones == bestOnes && key < bestKey) {
				bestKey = key
				bestOnes = ones
			}
		}
		if bestKey != "" {
			if peers, ok := rt.entries[bestKey]; ok && len(peers) > 0 {
				return peers
			}
		}
	}
	return nil
}

// mergeKeys merges the route-table entries of the given keys into one
// per-peer list, most recently updated record winning per peer.
func (rt *RouteTable) mergeKeys(keys []string) []PeerLatency {
	// Merge per peer: the most recently updated record wins. Probes for
	// different ports of the same host may disagree; the freshest
	// measurement is the best signal, and picking by UpdatedAt keeps the
	// decision reproducible.
	merged := make(map[string]PeerLatency)
	for _, key := range keys {
		for _, p := range rt.entries[key] {
			prev, seen := merged[p.PeerName]
			if !seen || !p.UpdatedAt.Before(prev.UpdatedAt) {
				merged[p.PeerName] = p
			}
		}
	}

	peers := make([]PeerLatency, 0, len(merged))
	for _, p := range merged {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerName < peers[j].PeerName })
	return peers
}

// Prune removes route table entries where all peers are stale.
func (rt *RouteTable) Prune() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	now := time.Now()
	pruned := 0
	for key, peers := range rt.entries {
		allStale := true
		for _, p := range peers {
			if now.Sub(p.UpdatedAt) <= staleThreshold {
				allStale = false
				break
			}
		}
		if allStale {
			delete(rt.entries, key)
			delete(rt.cidrNets, key)
			pruned++
		}
	}
	return pruned
}

// GetAllEntries returns a snapshot of the route table for monitoring.
func (rt *RouteTable) GetAllEntries() map[string][]PeerLatency {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	result := make(map[string][]PeerLatency, len(rt.entries))
	for k, v := range rt.entries {
		copied := make([]PeerLatency, len(v))
		copy(copied, v)
		result[k] = copied
	}
	return result
}
