package cluster

import (
	"math"
	"net"
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
type RouteTable struct {
	mu             sync.RWMutex
	entries        map[string][]PeerLatency // key: "host:port"
	localName      string
	relayThreshold time.Duration
}

func NewRouteTable(localName string, relayThresholdMs int) *RouteTable {
	return &RouteTable{
		entries:        make(map[string][]PeerLatency),
		localName:      localName,
		relayThreshold: time.Duration(relayThresholdMs) * time.Millisecond,
	}
}

// Update records a probe result for a peer→target pair.
// Uses EWMA smoothing to dampen transient spikes.
func (rt *RouteTable) Update(targetKey string, peerName string, rtt time.Duration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

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

// BestExit returns the best peer for a target IP.
// Returns ("", 0) if local is already optimal or no data available.
// When local is unreachable but a peer can reach the target, it returns
// that peer — this is the core cluster routing use case (bypassing blocks).
func (rt *RouteTable) BestExit(targetIP string) (peerName string, relayGain time.Duration) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	peers := rt.match(targetIP)
	if len(peers) == 0 {
		return "", 0
	}

	var localRTT time.Duration
	localFound := false
	localAvailable := false
	var bestPeer PeerLatency
	bestPeer.RTT = time.Duration(math.MaxInt64)

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
		if p.Available && p.RTT < bestPeer.RTT {
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

// FastestPeer returns the fastest available peer for a target IP,
// ignoring local node entirely. Used by ForceRelay mode.
func (rt *RouteTable) FastestPeer(targetIP string) string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	peers := rt.match(targetIP)
	now := time.Now()
	var best PeerLatency
	best.RTT = time.Duration(math.MaxInt64)

	for _, p := range peers {
		if now.Sub(p.UpdatedAt) > staleThreshold {
			continue
		}
		if p.PeerName == rt.localName {
			continue
		}
		if p.Available && p.RTT < best.RTT {
			best = p
		}
	}
	return best.PeerName
}

// match finds entries for a target. Tries exact match first,
// then falls back to CIDR prefix matching.
func (rt *RouteTable) match(targetIP string) []PeerLatency {
	// Try exact "ip:port" keys first
	for key, peers := range rt.entries {
		host, _, err := net.SplitHostPort(key)
		if err != nil {
			host = key
		}
		if host == targetIP {
			return peers
		}
	}
	return nil
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
