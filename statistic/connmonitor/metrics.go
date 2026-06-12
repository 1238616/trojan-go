package connmonitor

import (
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// CloseReason categorises why a relayed connection ended. The hot path uses
// this to attribute disconnects so the dashboard can highlight abnormal
// patterns (auth failures, peer resets, etc.) instead of only showing a
// flat "closed" state.
type CloseReason int32

const (
	CloseReasonEOF CloseReason = iota
	CloseReasonTimeout
	CloseReasonReset
	CloseReasonAuthFail
	CloseReasonOther
)

func (r CloseReason) String() string {
	switch r {
	case CloseReasonEOF:
		return "eof"
	case CloseReasonTimeout:
		return "timeout"
	case CloseReasonReset:
		return "reset"
	case CloseReasonAuthFail:
		return "auth_fail"
	default:
		return "other"
	}
}

// HandshakeKind identifies which handshake family a sample belongs to.
// Currently only TLS handshake is recorded, but the enum leaves room for
// trojan auth, websocket upgrade, etc.
type HandshakeKind int32

const (
	HandshakeTLS HandshakeKind = iota
)

// OriginDialKind classifies the outcome of a server -> origin dial
// attempt so the dashboard can break down failures by root cause.
type OriginDialKind int32

const (
	OriginDialOK OriginDialKind = iota
	OriginDialDNS
	OriginDialRefused
	OriginDialTimeout
	OriginDialUnreachable
	OriginDialOther
)

func (k OriginDialKind) String() string {
	switch k {
	case OriginDialOK:
		return "ok"
	case OriginDialDNS:
		return "dns"
	case OriginDialRefused:
		return "refused"
	case OriginDialTimeout:
		return "timeout"
	case OriginDialUnreachable:
		return "unreachable"
	default:
		return "other"
	}
}

const (
	// metricsHistogramSampleCap caps the reservoir used for percentile
	// estimation to keep memory bounded under sustained traffic. 4 KiB of
	// float64 ~= 32 KiB resident, which is negligible.
	metricsHistogramSampleCap = 4096
)

// reservoir is a lock-protected sliding sample of float64 observations used
// for cheap P50/P95 percentile estimation. It is intentionally simple
// (random replacement past capacity) rather than t-digest because a single
// metrics tick reads it at most a few times per second.
type reservoir struct {
	mu   sync.Mutex
	data []float64
	n    uint64 // total observations seen, used for replacement decision
}

func newReservoir(cap int) *reservoir {
	return &reservoir{data: make([]float64, 0, cap)}
}

func (r *reservoir) add(v float64) {
	r.mu.Lock()
	r.n++
	if len(r.data) < cap(r.data) {
		r.data = append(r.data, v)
	} else {
		// Reservoir replacement: replace a random slot. We use n as the
		// replacement index modulo capacity to avoid pulling in math/rand
		// on the hot path; the resulting bias is acceptable for a pure
		// observability signal.
		r.data[int(r.n)%cap(r.data)] = v
	}
	r.mu.Unlock()
}

func (r *reservoir) percentiles(ps ...float64) []float64 {
	r.mu.Lock()
	if len(r.data) == 0 {
		r.mu.Unlock()
		out := make([]float64, len(ps))
		return out
	}
	snap := make([]float64, len(r.data))
	copy(snap, r.data)
	r.mu.Unlock()

	sort.Float64s(snap)
	out := make([]float64, len(ps))
	for i, p := range ps {
		idx := int(math.Ceil(p*float64(len(snap)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(snap) {
			idx = len(snap) - 1
		}
		out[i] = snap[idx]
	}
	return out
}

// ChannelGauge is a tiny pluggable accessor that reports a channel's
// current depth and capacity so the dashboard can visualise back-pressure
// without each tunnel having to import the metrics package.
type ChannelGauge struct {
	Name  string
	Depth func() int
	Cap   func() int
}

// Metrics is the singleton collector for the dashboard. All counters use
// atomics so the data path stays lock-free.
type Metrics struct {
	// ---- connection lifecycle ----
	connOpenTotal  atomic.Uint64
	connCloseTotal atomic.Uint64
	closeByReason  [5]atomic.Uint64

	// ---- per-second throughput histogram (bytes/second per connection) ----
	upBpsRes   *reservoir
	downBpsRes *reservoir

	// ---- TLS handshake ----
	tlsHandshakeTotal        atomic.Uint64
	tlsHandshakeFailed       atomic.Uint64
	tlsHandshakeResumedTotal atomic.Uint64 // Phase 2
	tlsHandshakeLatRes       *reservoir    // milliseconds

	// ---- origin (server -> upstream) dial ----
	originDialTotal      atomic.Uint64
	originDialFailByKind [6]atomic.Uint64 // indexed by OriginDialKind
	originDialLatRes     *reservoir       // milliseconds, all attempts
	ttfbLatRes           *reservoir       // milliseconds, dial-done -> first downstream byte

	// ---- trojan auth ----
	trojanAuthTotal     atomic.Uint64
	trojanAuthFailed    atomic.Uint64
	trojanAuthLatRes    *reservoir // milliseconds
	trojanAuthFailMu    sync.Mutex
	trojanAuthFailKinds map[string]uint64

	// ---- channel water-marks (registered by tunnels) ----
	gaugesMu sync.RWMutex
	gauges   []ChannelGauge

	// ---- rate snapshot updated by the connmonitor calcLoop ----
	lastOpenSnap  atomic.Uint64
	lastCloseSnap atomic.Uint64
	openCPS       atomic.Uint64 // connections/second, last completed second
	closeCPS      atomic.Uint64

	// ---- Phase 1: UDP / packet flow ----
	packetOpenTotal   atomic.Uint64
	packetCloseTotal  atomic.Uint64
	packetCloseReason [5]atomic.Uint64
	packetPpsRes      *reservoir // packets/second per active packet-flow
	packetBpsRes      *reservoir // bytes/second per active packet-flow

	// ---- Phase 1: DNS resolve latency (split out from origin dial) ----
	dnsResolveTotal  atomic.Uint64
	dnsResolveFailed atomic.Uint64
	dnsResolveLatRes *reservoir // milliseconds
	tcpDialLatRes    *reservoir // milliseconds (pure TCP connect, no DNS)

	// ---- Phase 1: back-pressure / accept drops ----
	acceptDropsTotal   atomic.Uint64
	backpressureEvents atomic.Uint64
	// backpressureThresh is the fractional channel fill level (0, 1]
	// above which a backpressure event is recorded. Stored as a float64
	// bit pattern so the calcLoop hot path can read it without locking.
	backpressureThresh atomic.Uint64

	// ---- Phase 2: per-target metrics ----
	targetsMu sync.Mutex
	targets   map[string]*perTargetMetrics
	targetCap int

	// ---- Phase 3: per-user metrics ----
	usersMu sync.Mutex
	users   map[string]*PerUserMetrics
	userCap int

	// ---- Phase 3: mux stream metrics ----
	muxStreamsActive     atomic.Int64
	muxStreamsTotal      atomic.Uint64
	muxStreamsPerConnRes *reservoir
	muxQueueDepthRes     *reservoir

	// ---- Phase 3: zero-copy splice ----
	spliceBytesTotal    atomic.Uint64
	spliceCallsTotal    atomic.Uint64
	spliceFallbackTotal atomic.Uint64

	// ---- Phase 3: socket telemetry ----
	tcpRttRes    *reservoir // microseconds
	tcpCwndRes   *reservoir // segments
	tcpLossTotal atomic.Uint64
}

var (
	globalMetrics     *Metrics
	globalMetricsOnce sync.Once
)

// GlobalMetrics returns the singleton metrics collector.
func GlobalMetrics() *Metrics {
	globalMetricsOnce.Do(func() {
		globalMetrics = &Metrics{
			upBpsRes:            newReservoir(metricsHistogramSampleCap),
			downBpsRes:          newReservoir(metricsHistogramSampleCap),
			tlsHandshakeLatRes:  newReservoir(metricsHistogramSampleCap),
			originDialLatRes:    newReservoir(metricsHistogramSampleCap),
			ttfbLatRes:          newReservoir(metricsHistogramSampleCap),
			trojanAuthLatRes:    newReservoir(metricsHistogramSampleCap),
			trojanAuthFailKinds: make(map[string]uint64, 8),
			// Phase 1
			packetPpsRes:     newReservoir(metricsHistogramSampleCap),
			packetBpsRes:     newReservoir(metricsHistogramSampleCap),
			dnsResolveLatRes: newReservoir(metricsHistogramSampleCap),
			tcpDialLatRes:    newReservoir(metricsHistogramSampleCap),
			// Phase 2
			targetCap: targetCapDefault,
			// Phase 3
			userCap:              userCapDefault,
			muxStreamsPerConnRes: newReservoir(metricsHistogramSampleCap),
			muxQueueDepthRes:     newReservoir(metricsHistogramSampleCap),
			tcpRttRes:            newReservoir(metricsHistogramSampleCap),
			tcpCwndRes:           newReservoir(metricsHistogramSampleCap),
		}
	})
	return globalMetrics
}

// RecordConnOpen is called when a new relay connection enters the proxy.
func (m *Metrics) RecordConnOpen() { m.connOpenTotal.Add(1) }

// RecordConnClose is called when a relay connection exits, with the reason
// inferred by the caller from the relay-loop error value.
func (m *Metrics) RecordConnClose(r CloseReason) {
	m.connCloseTotal.Add(1)
	if r < 0 || int(r) >= len(m.closeByReason) {
		r = CloseReasonOther
	}
	m.closeByReason[r].Add(1)
}

// RecordHandshake reports a TLS handshake outcome and its latency.
func (m *Metrics) RecordHandshake(kind HandshakeKind, ok bool, dur time.Duration) {
	if kind != HandshakeTLS {
		return
	}
	m.tlsHandshakeTotal.Add(1)
	if !ok {
		m.tlsHandshakeFailed.Add(1)
		return
	}
	// keep sub-millisecond precision instead of integer truncation
	m.tlsHandshakeLatRes.add(float64(dur) / float64(time.Millisecond))
}

// RecordTLSResumed increments the resumed-handshake counter.
func (m *Metrics) RecordTLSResumed() { m.tlsHandshakeResumedTotal.Add(1) }

// ---- Phase 3: mux stream metrics ----

// RecordMuxStreamOpen is called when a new mux stream is opened.
func (m *Metrics) RecordMuxStreamOpen() {
	m.muxStreamsActive.Add(1)
	m.muxStreamsTotal.Add(1)
}

// RecordMuxStreamClose is called when a mux stream is closed.
func (m *Metrics) RecordMuxStreamClose() {
	m.muxStreamsActive.Add(-1)
}

// ObserveMuxStreamsPerConn samples the current stream count per physical
// connection into the reservoir for P50/P95 estimation.
func (m *Metrics) ObserveMuxStreamsPerConn(n float64) {
	if n > 0 {
		m.muxStreamsPerConnRes.add(n)
	}
}

// ObserveMuxQueueDepth samples the mux queue depth.
func (m *Metrics) ObserveMuxQueueDepth(n float64) {
	if n >= 0 {
		m.muxQueueDepthRes.add(n)
	}
}

// ---- Phase 3: zero-copy splice ----

// RecordSplice reports a successful splice operation.
func (m *Metrics) RecordSplice(bytes int64) {
	m.spliceCallsTotal.Add(1)
	if bytes > 0 {
		m.spliceBytesTotal.Add(uint64(bytes))
	}
}

// RecordSpliceFallback reports a splice fallback to userspace copy.
func (m *Metrics) RecordSpliceFallback() { m.spliceFallbackTotal.Add(1) }

// ---- Phase 3: socket telemetry ----

// RecordTCPInfo samples a TCPInfo observation.
func (m *Metrics) RecordTCPInfo(rttUs, cwnd float64, loss uint32) {
	if rttUs > 0 {
		m.tcpRttRes.add(rttUs)
	}
	if cwnd > 0 {
		m.tcpCwndRes.add(cwnd)
	}
	m.tcpLossTotal.Add(uint64(loss))
}

// RecordOriginDial reports a server -> upstream dial outcome and latency.
// kind == OriginDialOK indicates a successful dial; any other kind indicates
// a failure root cause classified by the caller.
func (m *Metrics) RecordOriginDial(dur time.Duration, kind OriginDialKind) {
	m.originDialTotal.Add(1)
	if kind < 0 || int(kind) >= len(m.originDialFailByKind) {
		kind = OriginDialOther
	}
	if kind != OriginDialOK {
		m.originDialFailByKind[kind].Add(1)
	}
	m.originDialLatRes.add(float64(dur) / float64(time.Millisecond))
}

// RecordTTFB reports the latency between dial completion and the first
// non-zero downstream Read on the relay (i.e., the time the origin took to
// produce the first byte after the TCP connection was ready).
func (m *Metrics) RecordTTFB(dur time.Duration) {
	if dur <= 0 {
		return
	}
	m.ttfbLatRes.add(float64(dur) / float64(time.Millisecond))
}

// RecordTrojanAuth reports a trojan inbound auth outcome.
// failKind is a stable short token (e.g. "invalid_hash", "ip_limit",
// "read_hash", "read_crlf", "read_metadata"); ignored when ok == true.
func (m *Metrics) RecordTrojanAuth(ok bool, dur time.Duration, failKind string) {
	m.trojanAuthTotal.Add(1)
	m.trojanAuthLatRes.add(float64(dur) / float64(time.Millisecond))
	if ok {
		return
	}
	m.trojanAuthFailed.Add(1)
	if failKind == "" {
		failKind = "other"
	}
	m.trojanAuthFailMu.Lock()
	m.trojanAuthFailKinds[failKind]++
	m.trojanAuthFailMu.Unlock()
}

// RegisterChannelGauge lets a tunnel server expose its accept channel
// depth/capacity to the dashboard. Safe to call from any goroutine.
func (m *Metrics) RegisterChannelGauge(g ChannelGauge) {
	m.gaugesMu.Lock()
	m.gauges = append(m.gauges, g)
	m.gaugesMu.Unlock()
}

// ---- Phase 1 record methods (hot-path, atomic, lock-free) ----

// RecordPacketOpen is called when a new packet flow enters the relay.
func (m *Metrics) RecordPacketOpen() { m.packetOpenTotal.Add(1) }

// RecordPacketClose is called when a packet flow ends. reason is the
// same CloseReason enum used by TCP flows.
func (m *Metrics) RecordPacketClose(r CloseReason) {
	m.packetCloseTotal.Add(1)
	if r < 0 || int(r) >= len(m.packetCloseReason) {
		r = CloseReasonOther
	}
	m.packetCloseReason[r].Add(1)
}

// RecordDNSResolve reports the DNS lookup portion of an origin dial.
// host is used only for future per-target breakdown; ok == false bumps
// the dnsResolveFailed counter.
func (m *Metrics) RecordDNSResolve(dur time.Duration, ok bool, host string) {
	m.dnsResolveTotal.Add(1)
	if !ok {
		m.dnsResolveFailed.Add(1)
	}
	if dur > 0 {
		m.dnsResolveLatRes.add(float64(dur) / float64(time.Millisecond))
	}
}

// RecordTCPDial reports the pure-TCP connect portion of an origin dial
// (i.e. after DNS resolution).
func (m *Metrics) RecordTCPDial(dur time.Duration) {
	if dur > 0 {
		m.tcpDialLatRes.add(float64(dur) / float64(time.Millisecond))
	}
}

// RecordAcceptDrop is called when an inbound accept channel is full and
// the proxy is forced to discard the connection.
func (m *Metrics) RecordAcceptDrop() { m.acceptDropsTotal.Add(1) }

// RecordBackpressureEvent is called by calcLoop when a registered
// ChannelGauge exceeds the configured backpressure threshold.
func (m *Metrics) RecordBackpressureEvent() { m.backpressureEvents.Add(1) }

// SetBackpressureThresh configures the fractional channel-fill level
// (0, 1] above which RecordBackpressureEvent is fired from calcLoop.
// A value of 0 disables back-pressure accounting entirely.
func (m *Metrics) SetBackpressureThresh(frac float64) {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	m.backpressureThresh.Store(math.Float64bits(frac))
}

// BackpressureThresh returns the currently configured threshold.
func (m *Metrics) BackpressureThresh() float64 {
	return math.Float64frombits(m.backpressureThresh.Load())
}

// ObservePacketPerSecond is invoked once per second by Monitor.calcLoop
// with the aggregated per-flow pps and bps samples.
func (m *Metrics) ObservePacketPerSecond(pps, bps float64) {
	if pps > 0 {
		m.packetPpsRes.add(pps)
	}
	if bps > 0 {
		m.packetBpsRes.add(bps)
	}
}

// SnapshotChannels returns a copy of the registered gauges with current
// depth/cap values.
type ChannelSample struct {
	Name  string `json:"name"`
	Depth int    `json:"depth"`
	Cap   int    `json:"cap"`
}

func (m *Metrics) SnapshotChannels() []ChannelSample {
	m.gaugesMu.RLock()
	defer m.gaugesMu.RUnlock()
	out := make([]ChannelSample, 0, len(m.gauges))
	for _, g := range m.gauges {
		s := ChannelSample{Name: g.Name}
		if g.Depth != nil {
			s.Depth = g.Depth()
		}
		if g.Cap != nil {
			s.Cap = g.Cap()
		}
		out = append(out, s)
	}
	return out
}

// observePerSecond is invoked once per second by Monitor.calcLoop with the
// per-connection throughput delta. It feeds the throughput reservoirs and
// recomputes the connection-rate gauges.
func (m *Metrics) observePerSecond(perConnUpBps, perConnDownBps []float64) {
	for _, v := range perConnUpBps {
		if v > 0 {
			m.upBpsRes.add(v)
		}
	}
	for _, v := range perConnDownBps {
		if v > 0 {
			m.downBpsRes.add(v)
		}
	}
	openNow := m.connOpenTotal.Load()
	closeNow := m.connCloseTotal.Load()
	m.openCPS.Store(openNow - m.lastOpenSnap.Swap(openNow))
	m.closeCPS.Store(closeNow - m.lastCloseSnap.Swap(closeNow))
}

// MetricsSnapshot is the JSON payload exposed to the dashboard.
type MetricsSnapshot struct {
	// Lifecycle
	ConnOpenTotal  uint64            `json:"conn_open_total"`
	ConnCloseTotal uint64            `json:"conn_close_total"`
	OpenCPS        uint64            `json:"open_cps"`
	CloseCPS       uint64            `json:"close_cps"`
	CloseByReason  map[string]uint64 `json:"close_by_reason"`

	// Throughput percentiles, bytes per second per connection
	UpBpsP50   float64 `json:"up_bps_p50"`
	UpBpsP95   float64 `json:"up_bps_p95"`
	DownBpsP50 float64 `json:"down_bps_p50"`
	DownBpsP95 float64 `json:"down_bps_p95"`

	// TLS handshake
	TLSHandshakeTotal        uint64  `json:"tls_handshake_total"`
	TLSHandshakeFailed       uint64  `json:"tls_handshake_failed"`
	TLSHandshakeResumedTotal uint64  `json:"tls_handshake_resumed_total"`
	TLSHandshakeP50Ms        float64 `json:"tls_handshake_p50_ms"`
	TLSHandshakeP95Ms        float64 `json:"tls_handshake_p95_ms"`

	// Origin dial (server -> upstream)
	OriginDialTotal      uint64            `json:"origin_dial_total"`
	OriginDialFailed     uint64            `json:"origin_dial_failed"`
	OriginDialFailByKind map[string]uint64 `json:"origin_dial_fail_by_kind"`
	OriginDialP50Ms      float64           `json:"origin_dial_p50_ms"`
	OriginDialP95Ms      float64           `json:"origin_dial_p95_ms"`

	// Time-to-first-byte (origin -> client first byte after dial)
	TTFBP50Ms float64 `json:"ttfb_p50_ms"`
	TTFBP95Ms float64 `json:"ttfb_p95_ms"`

	// Trojan inbound auth
	TrojanAuthTotal      uint64            `json:"trojan_auth_total"`
	TrojanAuthFailed     uint64            `json:"trojan_auth_failed"`
	TrojanAuthP50Ms      float64           `json:"trojan_auth_p50_ms"`
	TrojanAuthP95Ms      float64           `json:"trojan_auth_p95_ms"`
	TrojanAuthFailByKind map[string]uint64 `json:"trojan_auth_fail_by_kind"`

	// Channel water-marks
	Channels []ChannelSample `json:"channels"`

	// Runtime
	Goroutines    int     `json:"goroutines"`
	HeapAllocMB   float64 `json:"heap_alloc_mb"`
	HeapSysMB     float64 `json:"heap_sys_mb"`
	NumGC         uint32  `json:"num_gc"`
	GCPauseLastMs float64 `json:"gc_pause_last_ms"`
	GCCPUFraction float64 `json:"gc_cpu_fraction"`

	// ---- Phase 1: UDP / packet flow ----
	PacketOpenTotal     uint64            `json:"packet_open_total"`
	PacketCloseTotal    uint64            `json:"packet_close_total"`
	PacketCloseByReason map[string]uint64 `json:"packet_close_by_reason"`
	PacketPpsP50        float64           `json:"packet_pps_p50"`
	PacketPpsP95        float64           `json:"packet_pps_p95"`
	PacketBpsP50        float64           `json:"packet_bps_p50"`
	PacketBpsP95        float64           `json:"packet_bps_p95"`

	// ---- Phase 1: DNS resolve latency (split out from origin dial) ----
	DNSResolveTotal  uint64  `json:"dns_resolve_total"`
	DNSResolveFailed uint64  `json:"dns_resolve_failed"`
	DNSResolveP50Ms  float64 `json:"dns_resolve_p50_ms"`
	DNSResolveP95Ms  float64 `json:"dns_resolve_p95_ms"`
	TCPDialP50Ms     float64 `json:"tcp_dial_p50_ms"`
	TCPDialP95Ms     float64 `json:"tcp_dial_p95_ms"`

	// ---- Phase 1: sync.Pool health ----
	Pools []PoolSample `json:"pools,omitempty"`

	// ---- Phase 1: back-pressure / accept drops ----
	AcceptDropsTotal   uint64 `json:"accept_drops_total"`
	BackpressureEvents uint64 `json:"backpressure_events"`

	// ---- Phase 2: per-target breakdown ----
	TopTargets []TargetSample `json:"top_targets,omitempty"`

	// ---- Phase 3: per-user breakdown ----
	Users []UserSample `json:"users,omitempty"`

	// ---- Phase 3: mux stream metrics ----
	MuxStreamsActive     uint64  `json:"mux_streams_active"`
	MuxStreamsTotal      uint64  `json:"mux_streams_total"`
	MuxStreamsPerConnP50 float64 `json:"mux_streams_per_conn_p50"`
	MuxStreamsPerConnP95 float64 `json:"mux_streams_per_conn_p95"`
	MuxQueueDepthP50     float64 `json:"mux_queue_depth_p50"`

	// ---- Phase 3: zero-copy splice ----
	SpliceBytesTotal    uint64 `json:"splice_bytes_total"`
	SpliceCallsTotal    uint64 `json:"splice_calls_total"`
	SpliceFallbackTotal uint64 `json:"splice_fallback_total"`

	// ---- Phase 3: socket telemetry ----
	TCPRttP50Us  float64 `json:"tcp_rtt_p50_us"`
	TCPRttP95Us  float64 `json:"tcp_rtt_p95_us"`
	TCPCwndP50   float64 `json:"tcp_cwnd_p50"`
	TCPCwndP95   float64 `json:"tcp_cwnd_p95"`
	TCPLossTotal uint64  `json:"tcp_loss_total"`
}

// Snapshot returns a point-in-time view of all metrics for the dashboard.
func (m *Metrics) Snapshot() MetricsSnapshot {
	upPct := m.upBpsRes.percentiles(0.50, 0.95)
	downPct := m.downBpsRes.percentiles(0.50, 0.95)
	tlsPct := m.tlsHandshakeLatRes.percentiles(0.50, 0.95)
	originPct := m.originDialLatRes.percentiles(0.50, 0.95)
	ttfbPct := m.ttfbLatRes.percentiles(0.50, 0.95)
	authPct := m.trojanAuthLatRes.percentiles(0.50, 0.95)
	// Phase 1 reservoirs
	pktPpsPct := m.packetPpsRes.percentiles(0.50, 0.95)
	pktBpsPct := m.packetBpsRes.percentiles(0.50, 0.95)
	dnsPct := m.dnsResolveLatRes.percentiles(0.50, 0.95)
	tcpPct := m.tcpDialLatRes.percentiles(0.50, 0.95)
	// Phase 3 reservoirs
	muxPerConnPct := m.muxStreamsPerConnRes.percentiles(0.50, 0.95)
	muxQDepthPct := m.muxQueueDepthRes.percentiles(0.50, 0.95)
	tcpRttPct := m.tcpRttRes.percentiles(0.50, 0.95)
	tcpCwndPct := m.tcpCwndRes.percentiles(0.50, 0.95)

	closeMap := make(map[string]uint64, len(m.closeByReason))
	for i := range m.closeByReason {
		closeMap[CloseReason(i).String()] = m.closeByReason[i].Load()
	}

	pktCloseMap := make(map[string]uint64, len(m.packetCloseReason))
	for i := range m.packetCloseReason {
		pktCloseMap[CloseReason(i).String()] = m.packetCloseReason[i].Load()
	}

	originFailMap := make(map[string]uint64, len(m.originDialFailByKind))
	var originFailedTotal uint64
	for i := range m.originDialFailByKind {
		v := m.originDialFailByKind[i].Load()
		if i == int(OriginDialOK) {
			continue
		}
		originFailMap[OriginDialKind(i).String()] = v
		originFailedTotal += v
	}

	authFailMap := make(map[string]uint64, 8)
	m.trojanAuthFailMu.Lock()
	for k, v := range m.trojanAuthFailKinds {
		authFailMap[k] = v
	}
	m.trojanAuthFailMu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	lastPause := float64(0)
	if ms.NumGC > 0 {
		lastPause = float64(ms.PauseNs[(ms.NumGC+255)%256]) / 1e6
	}

	return MetricsSnapshot{
		ConnOpenTotal:            m.connOpenTotal.Load(),
		ConnCloseTotal:           m.connCloseTotal.Load(),
		OpenCPS:                  m.openCPS.Load(),
		CloseCPS:                 m.closeCPS.Load(),
		CloseByReason:            closeMap,
		UpBpsP50:                 upPct[0],
		UpBpsP95:                 upPct[1],
		DownBpsP50:               downPct[0],
		DownBpsP95:               downPct[1],
		TLSHandshakeTotal:        m.tlsHandshakeTotal.Load(),
		TLSHandshakeFailed:       m.tlsHandshakeFailed.Load(),
		TLSHandshakeResumedTotal: m.tlsHandshakeResumedTotal.Load(),
		TLSHandshakeP50Ms:        tlsPct[0],
		TLSHandshakeP95Ms:        tlsPct[1],
		OriginDialTotal:          m.originDialTotal.Load(),
		OriginDialFailed:         originFailedTotal,
		OriginDialFailByKind:     originFailMap,
		OriginDialP50Ms:          originPct[0],
		OriginDialP95Ms:          originPct[1],
		TTFBP50Ms:                ttfbPct[0],
		TTFBP95Ms:                ttfbPct[1],
		TrojanAuthTotal:          m.trojanAuthTotal.Load(),
		TrojanAuthFailed:         m.trojanAuthFailed.Load(),
		TrojanAuthP50Ms:          authPct[0],
		TrojanAuthP95Ms:          authPct[1],
		TrojanAuthFailByKind:     authFailMap,
		Channels:                 m.SnapshotChannels(),
		Goroutines:               runtime.NumGoroutine(),
		HeapAllocMB:              float64(ms.HeapAlloc) / 1024 / 1024,
		HeapSysMB:                float64(ms.HeapSys) / 1024 / 1024,
		NumGC:                    ms.NumGC,
		GCPauseLastMs:            lastPause,
		GCCPUFraction:            ms.GCCPUFraction,
		// Phase 1
		PacketOpenTotal:     m.packetOpenTotal.Load(),
		PacketCloseTotal:    m.packetCloseTotal.Load(),
		PacketCloseByReason: pktCloseMap,
		PacketPpsP50:        pktPpsPct[0],
		PacketPpsP95:        pktPpsPct[1],
		PacketBpsP50:        pktBpsPct[0],
		PacketBpsP95:        pktBpsPct[1],
		DNSResolveTotal:     m.dnsResolveTotal.Load(),
		DNSResolveFailed:    m.dnsResolveFailed.Load(),
		DNSResolveP50Ms:     dnsPct[0],
		DNSResolveP95Ms:     dnsPct[1],
		TCPDialP50Ms:        tcpPct[0],
		TCPDialP95Ms:        tcpPct[1],
		Pools:               SnapshotPools(),
		AcceptDropsTotal:    m.acceptDropsTotal.Load(),
		BackpressureEvents:  m.backpressureEvents.Load(),
		// Phase 2
		TopTargets: m.SnapshotTargets(10),
		// Phase 3
		Users:                m.SnapshotUsers(10),
		MuxStreamsActive:     uint64(m.muxStreamsActive.Load()),
		MuxStreamsTotal:      m.muxStreamsTotal.Load(),
		MuxStreamsPerConnP50: muxPerConnPct[0],
		MuxStreamsPerConnP95: muxPerConnPct[1],
		MuxQueueDepthP50:     muxQDepthPct[0],
		SpliceBytesTotal:     m.spliceBytesTotal.Load(),
		SpliceCallsTotal:     m.spliceCallsTotal.Load(),
		SpliceFallbackTotal:  m.spliceFallbackTotal.Load(),
		TCPRttP50Us:          tcpRttPct[0],
		TCPRttP95Us:          tcpRttPct[1],
		TCPCwndP50:           tcpCwndPct[0],
		TCPCwndP95:           tcpCwndPct[1],
		TCPLossTotal:         m.tcpLossTotal.Load(),
	}
}
