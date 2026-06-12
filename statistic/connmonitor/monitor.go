package connmonitor

import (
	"math"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ConnInfo holds real-time stats for a single connection.
type ConnInfo struct {
	ID            string  `json:"id"`
	Target        string  `json:"target"`
	UploadBytes   int64   `json:"upload_bytes"`
	DownloadBytes int64   `json:"download_bytes"`
	UploadSpeed   float64 `json:"upload_speed"`   // bytes per second
	DownloadSpeed float64 `json:"download_speed"` // bytes per second
	StartTime     int64   `json:"start_time"`     // unix timestamp
	Duration      float64 `json:"duration"`       // seconds
	Status        string  `json:"status"`         // "active" or "closed"
}

// Summary provides aggregate stats across all active connections.
type Summary struct {
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
	target    string
	startTime time.Time

	// status: 0 = active, 1 = closed
	statusFlag atomic.Int32

	uploadBytes   atomic.Int64
	downloadBytes atomic.Int64

	// updated by calcLoop only
	lastUpload    atomic.Int64
	lastDownload  atomic.Int64
	uploadSpeed   atomic.Uint64 // bits of float64
	downloadSpeed atomic.Uint64

	// scheduled deletion timestamp (unix nano), 0 = not scheduled
	deleteAfter atomic.Int64

	// Phase 3: raw TCP connection for TCPInfo sampling (nil when
	// not set or not a *net.TCPConn). Protected by atomic.Pointer
	// so calcLoop can read without locking.
	rawConn atomic.Value // stores *net.TCPConn or nil
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

func (e *connEntry) statusString() string {
	if e.statusFlag.Load() == 0 {
		return "active"
	}
	return "closed"
}

// SetRawConn stores a reference to the underlying *net.TCPConn so the
// calcLoop can periodically sample TCPInfo. Safe to call once after
// RegisterEntry; the value is read atomically by calcLoop.
func (e *connEntry) SetRawConn(c *net.TCPConn) {
	if e == nil {
		return
	}
	e.rawConn.Store(c)
}

// RawConn returns the stored *net.TCPConn, or nil.
func (e *connEntry) RawConn() *net.TCPConn {
	if e == nil {
		return nil
	}
	v := e.rawConn.Load()
	if v == nil {
		return nil
	}
	return v.(*net.TCPConn)
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
	e := &connEntry{
		id:        id,
		target:    target,
		startTime: time.Now(),
	}
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
	result := make([]ConnInfo, 0, m.totalCount.Load())
	m.connections.Range(func(_, v interface{}) bool {
		e := v.(*connEntry)
		result = append(result, ConnInfo{
			ID:            e.id,
			Target:        e.target,
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
	var tickCount int
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			tickCount++
			now := time.Now()
			nowNano := now.UnixNano()
			var totalUp, totalDown float64
			var activeCount int
			// Per-second per-connection samples fed into the metrics
			// reservoirs for P50/P95 throughput estimation.
			var upSamples, downSamples []float64

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
				upSamples = append(upSamples, up)
				downSamples = append(downSamples, down)
				return true
			})

			// Feed metrics collector (no-op when nobody asked for it yet).
			if globalMetrics != nil {
				globalMetrics.observePerSecond(upSamples, downSamples)
				// Phase 1: sample registered ChannelGauges and bump the
				// backpressure event counter when any channel is above
				// the configured threshold.
				if thresh := globalMetrics.BackpressureThresh(); thresh > 0 {
					for _, s := range globalMetrics.SnapshotChannels() {
						if s.Cap > 0 && float64(s.Depth)/float64(s.Cap) >= thresh {
							globalMetrics.RecordBackpressureEvent()
						}
					}
				}
				// Phase 3: sample TCPInfo every 5 seconds for entries
				// that have a raw TCP connection reference set.
				if tickCount%5 == 0 {
					m.connections.Range(func(_, v interface{}) bool {
						ent := v.(*connEntry)
						if ent.statusFlag.Load() != 0 {
							return true // skip closed
						}
						tc := ent.RawConn()
						if tc == nil {
							return true
						}
						ss, err := SampleTCPInfo(tc)
						if err != nil || ss == nil {
							return true
						}
						globalMetrics.RecordTCPInfo(
							float64(ss.RTTUs),
							float64(ss.SndCwnd),
							ss.Loss,
						)
						return true
					})
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
