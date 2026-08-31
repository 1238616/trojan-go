package connmonitor

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ConnInfo holds real-time stats for a single connection.
type ConnInfo struct {
	ID            string  `json:"id"`
	Target        string  `json:"target"`
	Type          string  `json:"type"` // "tcp" or "udp"
	UploadBytes   int64   `json:"upload_bytes"`
	DownloadBytes int64   `json:"download_bytes"`
	UploadSpeed   float64 `json:"upload_speed"`   // bytes per second
	DownloadSpeed float64 `json:"download_speed"` // bytes per second
	StartTime     int64   `json:"start_time"`     // unix timestamp
	Duration      float64 `json:"duration"`       // seconds
	Status        string  `json:"status"`         // "active" or "closed"
}

// EntryKind distinguishes TCP connections from UDP packet flows so the
// dashboard and the per-second samplers can treat them separately
// (issue #10).
type EntryKind int

const (
	EntryTCP EntryKind = iota
	EntryUDP
)

func (k EntryKind) String() string {
	if k == EntryUDP {
		return "udp"
	}
	return "tcp"
}

// Summary provides aggregate stats across all active connections.
type Summary struct {
	// TotalConnections is CUMULATIVE: the number of connections registered
	// since process start. It never decreases. Use ActiveConnections for the
	// current count.
	TotalConnections   int     `json:"total_connections"`
	ActiveConnections  int     `json:"active_connections"`
	TotalUploadSpeed   float64 `json:"total_upload_speed"`
	TotalDownloadSpeed float64 `json:"total_download_speed"`
	TotalUploadBytes   int64   `json:"total_upload_bytes"`
	TotalDownloadBytes int64   `json:"total_download_bytes"`
}

// Entry is the per-connection bookkeeping object exported so that hot
// paths (e.g. the proxy relay) can hold a pointer obtained from
// RegisterEntry and update counters with zero locking.
//
// connEntry is kept as an internal alias name for backward references.
type Entry = connEntry

// connEntry is internal bookkeeping for one connection.
// All counters are atomic so the data path is lock-free.
type connEntry struct {
	id        string
	targetVal atomic.Value // string; UDP flows learn their target from the first packet
	startTime time.Time

	// kind is immutable after registration.
	kind EntryKind

	// status: 0 = active, 1 = closed
	statusFlag atomic.Int32

	uploadBytes   atomic.Int64
	downloadBytes atomic.Int64

	// packets counts datagrams relayed in both directions (UDP flows
	// only; TCP flows leave it at zero). Feeds the packet_pps metric.
	packets atomic.Int64

	// updated by calcLoop only
	lastUpload    atomic.Int64
	lastDownload  atomic.Int64
	lastPackets   atomic.Int64
	uploadSpeed   atomic.Uint64 // bits of float64
	downloadSpeed atomic.Uint64

	// targetSet guards SetTargetOnce so concurrent packet directions
	// don't race over which address labels the flow.
	targetSet atomic.Bool

	// scheduled deletion timestamp (unix nano), 0 = not scheduled
	deleteAfter atomic.Int64
}

// AddUpload is the lock-free hot path used by the relay.
func (e *connEntry) AddUpload(n int64) {
	if e == nil {
		return
	}
	e.uploadBytes.Add(n)
}

// AddDownload is the lock-free hot path used by the relay.
func (e *connEntry) AddDownload(n int64) {
	if e == nil {
		return
	}
	e.downloadBytes.Add(n)
}

// AddPacket counts one relayed datagram (UDP flows).
func (e *connEntry) AddPacket() {
	if e == nil {
		return
	}
	e.packets.Add(1)
}

// target returns the flow's current target string.
func (e *connEntry) target() string {
	if v := e.targetVal.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SetTargetOnce labels the flow with its first observed target (UDP
// flows register before the first packet reveals the destination).
// Only the first call wins.
func (e *connEntry) SetTargetOnce(s string) {
	if e == nil || s == "" {
		return
	}
	if e.targetSet.CompareAndSwap(false, true) {
		e.targetVal.Store(s)
	}
}

// TargetKnown reports whether the flow already carries a target label.
// Hot paths check this before paying for Address.String() on every
// packet; a nil entry reports true so SetTargetOnce attempts are skipped.
func (e *connEntry) TargetKnown() bool {
	if e == nil {
		return true
	}
	return e.targetSet.Load()
}

func (e *connEntry) statusString() string {
	if e.statusFlag.Load() == 0 {
		return "active"
	}
	return "closed"
}

// TrafficPoint represents a single point in the traffic history timeline.
type TrafficPoint struct {
	Timestamp     int64   `json:"timestamp"`      // unix timestamp
	UploadSpeed   float64 `json:"upload_speed"`   // bytes per second
	DownloadSpeed float64 `json:"download_speed"` // bytes per second
	ActiveConns   int     `json:"active_conns"`
}

const (
	historySize     = 900 // 15 minutes at 1-second intervals
	deleteDelayNano = int64(10 * time.Second)
)

// Monitor tracks all active connections and calculates per-connection speed.
type Monitor struct {
	connections sync.Map // map[string]*connEntry, lock-free hot path
	totalCount  atomic.Int64
	activeCount atomic.Int64

	historyMu  sync.RWMutex
	history    [historySize]TrafficPoint
	historyPos int
	historyLen int

	stopCh chan struct{}
	once   sync.Once
}

// NewMonitor creates a Monitor and starts the background speed calculator.
func NewMonitor() *Monitor {
	m := &Monitor{
		stopCh: make(chan struct{}),
	}
	go m.calcLoop()
	return m
}

// RegisterEntry adds a new connection and returns the *connEntry pointer.
// Hot paths should hold this pointer and call AddUpload / AddDownload directly,
// avoiding the map lookup performed by RecordUpload / RecordDownload.
func (m *Monitor) RegisterEntry(id, target string) *connEntry {
	return m.registerEntry(id, target, EntryTCP)
}

// RegisterPacketEntry adds a new UDP packet flow. The target may be
// unknown at registration time (it is learned from the first packet
// via SetTargetOnce); pass a placeholder such as "udp".
func (m *Monitor) RegisterPacketEntry(id, target string) *connEntry {
	return m.registerEntry(id, target, EntryUDP)
}

func (m *Monitor) registerEntry(id, target string, kind EntryKind) *connEntry {
	e := &connEntry{
		id:        id,
		startTime: time.Now(),
		kind:      kind,
	}
	e.targetVal.Store(target)
	m.connections.Store(id, e)
	m.totalCount.Add(1)
	m.activeCount.Add(1)
	return e
}

// Register adds a new connection to monitoring (legacy API).
func (m *Monitor) Register(id, target string) {
	m.RegisterEntry(id, target)
}

// Unregister marks a connection as closed and schedules removal.
// Removal happens in calcLoop, no per-call goroutine is started.
func (m *Monitor) Unregister(id string) {
	v, ok := m.connections.Load(id)
	if !ok {
		return
	}
	e := v.(*connEntry)
	if e.statusFlag.CompareAndSwap(0, 1) {
		m.activeCount.Add(-1)
	}
	e.uploadSpeed.Store(0)
	e.downloadSpeed.Store(0)
	e.deleteAfter.Store(time.Now().UnixNano() + deleteDelayNano)
}

// UnregisterEntry is a faster Unregister variant when caller already has the *connEntry.
func (m *Monitor) UnregisterEntry(e *connEntry) {
	if e == nil {
		return
	}
	if e.statusFlag.CompareAndSwap(0, 1) {
		m.activeCount.Add(-1)
	}
	e.uploadSpeed.Store(0)
	e.downloadSpeed.Store(0)
	e.deleteAfter.Store(time.Now().UnixNano() + deleteDelayNano)
}

// RecordUpload atomically adds uploaded bytes (legacy API; map lookup).
func (m *Monitor) RecordUpload(id string, n int64) {
	if v, ok := m.connections.Load(id); ok {
		v.(*connEntry).uploadBytes.Add(n)
	}
}

// RecordDownload atomically adds downloaded bytes (legacy API; map lookup).
func (m *Monitor) RecordDownload(id string, n int64) {
	if v, ok := m.connections.Load(id); ok {
		v.(*connEntry).downloadBytes.Add(n)
	}
}

// GetAll returns a snapshot of all tracked connections.
func (m *Monitor) GetAll() []ConnInfo {
	now := time.Now()
	// Size the pre-allocation after the CURRENT number of connections.
	// totalCount is a cumulative counter that never decreases; using it
	// here made every /api/connections request allocate a slice proportional
	// to the process lifetime connection count (issue #2).
	result := make([]ConnInfo, 0, m.activeCount.Load())
	m.connections.Range(func(_, v interface{}) bool {
		e := v.(*connEntry)
		result = append(result, ConnInfo{
			ID:            e.id,
			Target:        e.target(),
			Type:          e.kind.String(),
			UploadBytes:   e.uploadBytes.Load(),
			DownloadBytes: e.downloadBytes.Load(),
			UploadSpeed:   loadFloat64(&e.uploadSpeed),
			DownloadSpeed: loadFloat64(&e.downloadSpeed),
			StartTime:     e.startTime.Unix(),
			Duration:      now.Sub(e.startTime).Seconds(),
			Status:        e.statusString(),
		})
		return true
	})
	sort.Slice(result, func(i, j int) bool {
		if result[i].StartTime != result[j].StartTime {
			return result[i].StartTime < result[j].StartTime
		}
		return result[i].ID < result[j].ID
	})
	return result
}

// GetSummary returns aggregate stats.
func (m *Monitor) GetSummary() Summary {
	var s Summary
	s.TotalConnections = int(m.totalCount.Load())
	m.connections.Range(func(_, v interface{}) bool {
		e := v.(*connEntry)
		if e.statusFlag.Load() == 0 {
			s.ActiveConnections++
			s.TotalUploadSpeed += loadFloat64(&e.uploadSpeed)
			s.TotalDownloadSpeed += loadFloat64(&e.downloadSpeed)
		}
		s.TotalUploadBytes += e.uploadBytes.Load()
		s.TotalDownloadBytes += e.downloadBytes.Load()
		return true
	})
	return s
}

// Stop shuts down the monitor.
func (m *Monitor) Stop() {
	m.once.Do(func() { close(m.stopCh) })
}

// GetHistory returns the traffic history for the last 15 minutes.
func (m *Monitor) GetHistory() []TrafficPoint {
	m.historyMu.RLock()
	defer m.historyMu.RUnlock()

	result := make([]TrafficPoint, 0, m.historyLen)
	start := (m.historyPos - m.historyLen + historySize) % historySize
	for i := 0; i < m.historyLen; i++ {
		idx := (start + i) % historySize
		result = append(result, m.history[idx])
	}
	return result
}

func (m *Monitor) calcLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			nowNano := now.UnixNano()
			var totalUp, totalDown float64
			var activeCount int
			// Per-second per-connection samples fed into the metrics
			// reservoirs for P50/P95 throughput estimation.
			var upSamples, downSamples []float64
			// Per-second per-UDP-flow samples (packets and bytes) fed
			// into the packet_pps / packet_bps reservoirs (issue #10).
			var pktPps, pktBps []float64

			m.connections.Range(func(k, v interface{}) bool {
				e := v.(*connEntry)
				// Reap expired closed entries.
				if e.statusFlag.Load() == 1 {
					if d := e.deleteAfter.Load(); d > 0 && nowNano >= d {
						m.connections.Delete(k)
					}
					return true
				}
				curUp := e.uploadBytes.Load()
				curDown := e.downloadBytes.Load()
				lastUp := e.lastUpload.Swap(curUp)
				lastDown := e.lastDownload.Swap(curDown)
				up := float64(curUp - lastUp)
				down := float64(curDown - lastDown)
				storeFloat64(&e.uploadSpeed, up)
				storeFloat64(&e.downloadSpeed, down)
				totalUp += up
				totalDown += down
				activeCount++
				if e.kind == EntryUDP {
					curPkts := e.packets.Load()
					lastPkts := e.lastPackets.Swap(curPkts)
					pktPps = append(pktPps, float64(curPkts-lastPkts))
					pktBps = append(pktBps, up+down)
				} else {
					upSamples = append(upSamples, up)
					downSamples = append(downSamples, down)
				}
				return true
			})

			// Feed metrics collector (no-op when nobody asked for it yet).
			// The pointer is loaded atomically: GlobalMetrics() may still
			// be initialising it concurrently on first use (issue #10).
			if gm := globalMetricsPtr.Load(); gm != nil {
				gm.observePerSecond(upSamples, downSamples)
				for i := range pktPps {
					gm.ObservePacketPerSecond(pktPps[i], pktBps[i])
				}
				// Phase 1: sample registered ChannelGauges and bump the
				// backpressure event counter when any channel is above
				// the configured threshold.
				if thresh := gm.BackpressureThresh(); thresh > 0 {
					for _, s := range gm.SnapshotChannels() {
						if s.Cap > 0 && float64(s.Depth)/float64(s.Cap) >= thresh {
							gm.RecordBackpressureEvent()
						}
					}
				}
			}

			m.historyMu.Lock()
			m.history[m.historyPos] = TrafficPoint{
				Timestamp:     now.Unix(),
				UploadSpeed:   totalUp,
				DownloadSpeed: totalDown,
				ActiveConns:   activeCount,
			}
			m.historyPos = (m.historyPos + 1) % historySize
			if m.historyLen < historySize {
				m.historyLen++
			}
			m.historyMu.Unlock()
		}
	}
}

// --- helpers: store/load float64 via atomic.Uint64 ---

func storeFloat64(p *atomic.Uint64, v float64) {
	p.Store(math.Float64bits(v))
}

func loadFloat64(p *atomic.Uint64) float64 {
	return math.Float64frombits(p.Load())
}
