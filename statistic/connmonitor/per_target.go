package connmonitor

import (
	"sort"
	"sync/atomic"
	"time"
)

// perTargetMetrics aggregates counters for one outbound host (the
// "target" in "server -> target"). The design keeps the hot path
// lock-free: atomic counters plus a per-host reservoir for dial
// latency percentiles.
//
// The process-global target map is capped at Metrics.targetCap hosts.
// When the cap is hit, excess hosts are merged into a synthetic
// "__other__" bucket so memory stays bounded in the presence of DNS
// rebinding or malicious host churn.
type perTargetMetrics struct {
	host           string
	dialTotal      atomic.Uint64
	dialFailTotal  atomic.Uint64
	dialLatRes     *reservoir // milliseconds
	bytesUpTotal   atomic.Uint64
	bytesDownTotal atomic.Uint64
	lastActive     atomic.Int64 // unix seconds
}

// RecordDial reports one origin dial attempt for this target. ok == false
// bumps the fail counter. dur is the dial latency.
func (t *perTargetMetrics) RecordDial(dur time.Duration, ok bool) {
	if t == nil {
		return
	}
	t.dialTotal.Add(1)
	if !ok {
		t.dialFailTotal.Add(1)
	}
	if dur > 0 {
		t.dialLatRes.add(float64(dur) / float64(time.Millisecond))
	}
	t.lastActive.Store(time.Now().Unix())
}

// RecordUp / RecordDown bump per-target byte counters.
func (t *perTargetMetrics) RecordUp(n int64) {
	if t == nil || n <= 0 {
		return
	}
	t.bytesUpTotal.Add(uint64(n))
	t.lastActive.Store(time.Now().Unix())
}

func (t *perTargetMetrics) RecordDown(n int64) {
	if t == nil || n <= 0 {
		return
	}
	t.bytesDownTotal.Add(uint64(n))
	t.lastActive.Store(time.Now().Unix())
}

// TargetSample is the JSON-serialisable view of one target for the
// dashboard Top-Targets card.
type TargetSample struct {
	Host         string  `json:"host"`
	Dials        uint64  `json:"dials"`
	DialFails    uint64  `json:"dial_fails"`
	DialP50Ms    float64 `json:"dial_p50_ms"`
	DialP95Ms    float64 `json:"dial_p95_ms"`
	BytesUp      uint64  `json:"bytes_up"`
	BytesDown    uint64  `json:"bytes_down"`
	LastActiveAt int64   `json:"last_active_at"` // unix seconds
}

// Snapshot returns a point-in-time TargetSample.
func (t *perTargetMetrics) Snapshot() TargetSample {
	if t == nil {
		return TargetSample{}
	}
	pct := t.dialLatRes.percentiles(0.50, 0.95)
	return TargetSample{
		Host:         t.host,
		Dials:        t.dialTotal.Load(),
		DialFails:    t.dialFailTotal.Load(),
		DialP50Ms:    pct[0],
		DialP95Ms:    pct[1],
		BytesUp:      t.bytesUpTotal.Load(),
		BytesDown:    t.bytesDownTotal.Load(),
		LastActiveAt: t.lastActive.Load(),
	}
}

// targetCapDefault is used when Metrics is constructed without an
// explicit cap (e.g. legacy call sites that don't care).
const targetCapDefault = 128

// targetOtherKey is the synthetic bucket host for overflow.
const targetOtherKey = "__other__"

// SetTargetCap adjusts the per-host map capacity at startup. Values <= 0
// disable per-target accounting entirely.
func (m *Metrics) SetTargetCap(n int) {
	m.targetsMu.Lock()
	m.targetCap = n
	m.targetsMu.Unlock()
}

// GetTarget returns the per-target metrics for host, lazily creating
// the entry when capacity permits. When the map is full and host is
// new, the returned *perTargetMetrics points at the synthetic
// "__other__" bucket. When targetCap <= 0, GetTarget returns nil so
// that hot paths can cheaply short-circuit.
func (m *Metrics) GetTarget(host string) *perTargetMetrics {
	if m == nil {
		return nil
	}
	m.targetsMu.Lock()
	defer m.targetsMu.Unlock()
	if m.targetCap <= 0 {
		return nil
	}
	if m.targets == nil {
		m.targets = make(map[string]*perTargetMetrics, m.targetCap)
	}
	if t, ok := m.targets[host]; ok {
		return t
	}
	if len(m.targets) >= m.targetCap {
		host = targetOtherKey
		if t, ok := m.targets[host]; ok {
			return t
		}
	}
	t := &perTargetMetrics{host: host, dialLatRes: newReservoir(1024)}
	m.targets[host] = t
	return t
}

// SnapshotTargets returns the top-N targets sorted by BytesDown
// descending (so the dashboard's "Top Targets" card shows the
// heaviest consumers first).
func (m *Metrics) SnapshotTargets(limit int) []TargetSample {
	if m == nil || limit <= 0 {
		return nil
	}
	m.targetsMu.Lock()
	all := make([]TargetSample, 0, len(m.targets))
	for _, t := range m.targets {
		all = append(all, t.Snapshot())
	}
	m.targetsMu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		return all[i].BytesDown > all[j].BytesDown
	})
	if limit < len(all) {
		all = all[:limit]
	}
	return all
}
