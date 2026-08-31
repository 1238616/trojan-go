package connmonitor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestPoolStatsBasic verifies the hit-rate accounting in PoolStats: the
// first OnGet(true) is a miss, the following OnGet(false) calls are hits.
func TestPoolStatsBasic(t *testing.T) {
	p := &PoolStats{name: "unit"}
	p.OnGet(true)  // miss
	p.OnGet(false) // hit
	p.OnGet(false) // hit
	p.OnPut()
	p.OnPut()
	p.OnPut()
	s := p.Snapshot()
	if s.Gets != 3 {
		t.Errorf("Gets=%d, want 3", s.Gets)
	}
	if s.News != 1 {
		t.Errorf("News=%d, want 1", s.News)
	}
	if s.Puts != 3 {
		t.Errorf("Puts=%d, want 3", s.Puts)
	}
	want := 2.0 / 3.0
	if s.HitRate < want-1e-9 || s.HitRate > want+1e-9 {
		t.Errorf("HitRate=%v, want %v", s.HitRate, want)
	}
	if s.Name != "unit" {
		t.Errorf("Name=%q, want %q", s.Name, "unit")
	}
}

// TestPoolStatsZero ensures the hit-rate falls back to 0 when no Get
// has been observed.
func TestPoolStatsZero(t *testing.T) {
	p := &PoolStats{name: "empty"}
	if p.Snapshot().HitRate != 0 {
		t.Errorf("HitRate for empty pool should be 0, got %v", p.Snapshot().HitRate)
	}
}

// TestRegisterPoolIdempotent verifies that repeated RegisterPool calls
// with the same name return the same pointer.
func TestRegisterPoolIdempotent(t *testing.T) {
	a := RegisterPool("unit_test_pool_a")
	b := RegisterPool("unit_test_pool_a")
	if a != b {
		t.Fatalf("RegisterPool returned different pointers for same name")
	}
}

// TestMetricsPhase1Snapshot covers the Phase 1 fields added to
// MetricsSnapshot: packet open/close, DNS, TCP dial, pools, and
// backpressure.
func TestMetricsPhase1Snapshot(t *testing.T) {
	m := &Metrics{
		upBpsRes:            newReservoir(64),
		downBpsRes:          newReservoir(64),
		tlsHandshakeLatRes:  newReservoir(64),
		originDialLatRes:    newReservoir(64),
		ttfbLatRes:          newReservoir(64),
		trojanAuthLatRes:    newReservoir(64),
		trojanAuthFailKinds: make(map[string]uint64, 8),
		packetPpsRes:        newReservoir(64),
		packetBpsRes:        newReservoir(64),
		dnsResolveLatRes:    newReservoir(64),
		tcpDialLatRes:       newReservoir(64),
		// Phase 3 reservoirs (needed for Snapshot)
		muxStreamsPerConnRes: newReservoir(64),
		muxQueueDepthRes:     newReservoir(64),
	}
	// record some Phase 1 events
	m.RecordPacketOpen()
	m.RecordPacketOpen()
	m.RecordPacketClose(CloseReasonEOF)
	m.RecordPacketClose(CloseReasonTimeout)
	m.RecordDNSResolve(5*time.Millisecond, true, "a.test")
	m.RecordDNSResolve(10*time.Millisecond, true, "b.test")
	m.RecordDNSResolve(2*time.Millisecond, false, "c.test")
	m.RecordTCPDial(20 * time.Millisecond)
	m.RecordTCPDial(40 * time.Millisecond)
	m.ObservePacketPerSecond(1000, 8000)
	m.ObservePacketPerSecond(2000, 16000)
	m.RecordBackpressureEvent()

	snap := m.Snapshot()
	if snap.PacketOpenTotal != 2 {
		t.Errorf("PacketOpenTotal=%d, want 2", snap.PacketOpenTotal)
	}
	if snap.PacketCloseTotal != 2 {
		t.Errorf("PacketCloseTotal=%d, want 2", snap.PacketCloseTotal)
	}
	if snap.PacketCloseByReason["eof"] != 1 {
		t.Errorf("PacketCloseByReason[eof]=%v, want 1", snap.PacketCloseByReason["eof"])
	}
	if snap.PacketCloseByReason["timeout"] != 1 {
		t.Errorf("PacketCloseByReason[timeout]=%v, want 1", snap.PacketCloseByReason["timeout"])
	}
	if snap.DNSResolveTotal != 3 {
		t.Errorf("DNSResolveTotal=%d, want 3", snap.DNSResolveTotal)
	}
	if snap.DNSResolveFailed != 1 {
		t.Errorf("DNSResolveFailed=%d, want 1", snap.DNSResolveFailed)
	}
	if snap.BackpressureEvents != 1 {
		t.Errorf("BackpressureEvents=%d, want 1", snap.BackpressureEvents)
	}
	if snap.DNSResolveP50Ms <= 0 {
		t.Errorf("DNSResolveP50Ms=%v, want > 0", snap.DNSResolveP50Ms)
	}
	if snap.TCPDialP50Ms <= 0 {
		t.Errorf("TCPDialP50Ms=%v, want > 0", snap.TCPDialP50Ms)
	}
	if snap.PacketPpsP50 <= 0 {
		t.Errorf("PacketPpsP50=%v, want > 0", snap.PacketPpsP50)
	}

	// Snapshot must be JSON-encodable and contain the new field names.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal(snap) error: %v", err)
	}
	for _, key := range []string{
		"packet_open_total", "packet_close_total", "packet_close_by_reason",
		"dns_resolve_total", "dns_resolve_failed",
		"tcp_dial_p50_ms", "tcp_dial_p95_ms",
		"packet_pps_p50", "packet_bps_p50",
		"backpressure_events",
	} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("snapshot JSON missing key %q", key)
		}
	}
}

// TestBackpressureThreshAccessor validates the threshold setter/getter,
// including clamping behaviour at the edges of the (0, 1] range.
func TestBackpressureThreshAccessor(t *testing.T) {
	m := &Metrics{
		upBpsRes:            newReservoir(64),
		downBpsRes:          newReservoir(64),
		tlsHandshakeLatRes:  newReservoir(64),
		originDialLatRes:    newReservoir(64),
		ttfbLatRes:          newReservoir(64),
		trojanAuthLatRes:    newReservoir(64),
		trojanAuthFailKinds: make(map[string]uint64, 8),
		packetPpsRes:        newReservoir(64),
		packetBpsRes:        newReservoir(64),
		dnsResolveLatRes:    newReservoir(64),
		tcpDialLatRes:       newReservoir(64),
		// Phase 3 reservoirs (needed for Snapshot)
		muxStreamsPerConnRes: newReservoir(64),
		muxQueueDepthRes:     newReservoir(64),
	}
	m.SetBackpressureThresh(0.8)
	if got := m.BackpressureThresh(); got < 0.8-1e-9 || got > 0.8+1e-9 {
		t.Errorf("BackpressureThresh()=%v, want 0.8", got)
	}
	// clamping
	m.SetBackpressureThresh(-1)
	if got := m.BackpressureThresh(); got != 0 {
		t.Errorf("negative thresh should clamp to 0, got %v", got)
	}
	m.SetBackpressureThresh(5)
	if got := m.BackpressureThresh(); got != 1 {
		t.Errorf("over-1 thresh should clamp to 1, got %v", got)
	}
}

// TestRecordPacketCloseUnknownReason verifies that an out-of-range
// CloseReason is mapped to CloseReasonOther rather than panicking.
func TestRecordPacketCloseUnknownReason(t *testing.T) {
	m := &Metrics{
		upBpsRes:            newReservoir(64),
		downBpsRes:          newReservoir(64),
		tlsHandshakeLatRes:  newReservoir(64),
		originDialLatRes:    newReservoir(64),
		ttfbLatRes:          newReservoir(64),
		trojanAuthLatRes:    newReservoir(64),
		trojanAuthFailKinds: make(map[string]uint64, 8),
		packetPpsRes:        newReservoir(64),
		packetBpsRes:        newReservoir(64),
		dnsResolveLatRes:    newReservoir(64),
		tcpDialLatRes:       newReservoir(64),
		// Phase 3 reservoirs (needed for Snapshot)
		muxStreamsPerConnRes: newReservoir(64),
		muxQueueDepthRes:     newReservoir(64),
	}
	m.RecordPacketClose(CloseReason(99))
	snap := m.Snapshot()
	if snap.PacketCloseByReason[CloseReasonOther.String()] != 1 {
		t.Errorf("out-of-range reason not mapped to Other; snap=%+v", snap.PacketCloseByReason)
	}
}
