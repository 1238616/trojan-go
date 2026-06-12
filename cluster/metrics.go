package cluster

import (
	"sync"
	"sync/atomic"
	"time"
)

// ClusterMetrics tracks relay decisions, latency gains, and fallback events.
type ClusterMetrics struct {
	mu             sync.RWMutex
	totalRelays    int64
	totalFallbacks int64
	totalGainMs    int64 // cumulative gain in milliseconds

	// Per-peer relay counts
	peerRelays    map[string]*int64
	peerFallbacks map[string]*int64

	lastProbeAt   time.Time
	lastProbeDur  time.Duration
}

func NewClusterMetrics() *ClusterMetrics {
	return &ClusterMetrics{
		peerRelays:    make(map[string]*int64),
		peerFallbacks: make(map[string]*int64),
	}
}

func (m *ClusterMetrics) RecordRelay(peerName string, gain time.Duration) {
	atomic.AddInt64(&m.totalRelays, 1)
	atomic.AddInt64(&m.totalGainMs, gain.Milliseconds())

	m.mu.RLock()
	counter, ok := m.peerRelays[peerName]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		counter, ok = m.peerRelays[peerName]
		if !ok {
			var c int64
			counter = &c
			m.peerRelays[peerName] = counter
		}
		m.mu.Unlock()
	}
	atomic.AddInt64(counter, 1)
}

func (m *ClusterMetrics) RecordRelayFallback(peerName string) {
	atomic.AddInt64(&m.totalFallbacks, 1)

	m.mu.RLock()
	counter, ok := m.peerFallbacks[peerName]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		counter, ok = m.peerFallbacks[peerName]
		if !ok {
			var c int64
			counter = &c
			m.peerFallbacks[peerName] = counter
		}
		m.mu.Unlock()
	}
	atomic.AddInt64(counter, 1)
}

func (m *ClusterMetrics) RecordProbe(dur time.Duration) {
	m.mu.Lock()
	m.lastProbeAt = time.Now()
	m.lastProbeDur = dur
	m.mu.Unlock()
}

func (m *ClusterMetrics) Snapshot() ClusterStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	relays := atomic.LoadInt64(&m.totalRelays)
	var avgGain float64
	if relays > 0 {
		avgGain = float64(atomic.LoadInt64(&m.totalGainMs)) / float64(relays)
	}

	lastProbeAt := ""
	if !m.lastProbeAt.IsZero() {
		lastProbeAt = m.lastProbeAt.Format(time.RFC3339)
	}

	return ClusterStats{
		TotalRelays:    relays,
		TotalFallbacks: atomic.LoadInt64(&m.totalFallbacks),
		AvgGainMs:      avgGain,
		LastProbeAt:    lastProbeAt,
		LastProbeDurMs: float64(m.lastProbeDur.Microseconds()) / 1000.0,
	}
}

// --- API response types ---

type ClusterAPIResponse struct {
	Enabled          bool                  `json:"enabled"`
	LocalNode        string                `json:"local_node"`
	ProbeInterval    int                   `json:"probe_interval_sec"`
	LatencyThreshold int                   `json:"latency_threshold_ms"`
	Peers            []PeerStatus          `json:"peers"`
	OptimizedRoutes  []OptimizedRouteEntry `json:"optimized_routes"`
	Stats            ClusterStats          `json:"stats"`
}

type PeerStatus struct {
	Name        string      `json:"name"`
	Host        string      `json:"host"`
	Port        int         `json:"port"`
	Available   bool        `json:"available"`
	LastProbeAt string      `json:"last_probe_at"`
	TargetRTTs  []TargetRTT `json:"target_rtts"`
}

type TargetRTT struct {
	Target    string  `json:"target"`
	RTTMs     float64 `json:"rtt_ms"`
	RawRTTMs  float64 `json:"raw_rtt_ms"`
	Available bool    `json:"available"`
}

type OptimizedRouteEntry struct {
	Target      string  `json:"target"`
	LocalRTTMs  float64 `json:"local_rtt_ms"`
	BestPeer    string  `json:"best_peer"`
	PeerRTTMs   float64 `json:"peer_rtt_ms"`
	GainMs      float64 `json:"gain_ms"`
	GainPercent float64 `json:"gain_percent"`
	RelayCount  int64   `json:"relay_count"`
	Source      string  `json:"source"`
	FirstSeenAt string  `json:"first_seen_at"`
}

type ClusterStats struct {
	TotalRelays    int64   `json:"total_relays"`
	TotalFallbacks int64   `json:"total_fallbacks"`
	AvgGainMs      float64 `json:"avg_gain_ms"`
	ProbeTargets   int     `json:"probe_targets"`
	StaticTargets  int     `json:"static_targets"`
	DynamicTargets int     `json:"dynamic_targets"`
	LastProbeAt    string  `json:"last_probe_at"`
	LastProbeDurMs float64 `json:"last_probe_dur_ms"`
}
