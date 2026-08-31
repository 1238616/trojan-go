package connmonitor

import (
	"testing"
	"time"
)

// ---- per-target metrics ----

func TestPerTargetBasic(t *testing.T) {
	m := &Metrics{
		targetCap: 4,
	}
	a := m.GetTarget("a.test")
	b := m.GetTarget("b.test")
	if a == nil || b == nil {
		t.Fatal("GetTarget returned nil for cap=4")
	}
	a.RecordDial(10*time.Millisecond, true)
	a.RecordDial(20*time.Millisecond, true)
	a.RecordDial(5*time.Millisecond, false)
	a.RecordUp(100)
	a.RecordDown(200)
	b.RecordDial(50*time.Millisecond, true)
	b.RecordDown(500)

	snap := a.Snapshot()
	if snap.Host != "a.test" {
		t.Errorf("Host=%q, want a.test", snap.Host)
	}
	if snap.Dials != 3 {
		t.Errorf("Dials=%d, want 3", snap.Dials)
	}
	if snap.DialFails != 1 {
		t.Errorf("DialFails=%d, want 1", snap.DialFails)
	}
	if snap.BytesUp != 100 || snap.BytesDown != 200 {
		t.Errorf("Bytes up/down=%d/%d, want 100/200", snap.BytesUp, snap.BytesDown)
	}
}

func TestPerTargetCapOverflow(t *testing.T) {
	m := &Metrics{targetCap: 2}
	m.GetTarget("a")
	m.GetTarget("b")
	overflow := m.GetTarget("c")
	// Third unique host must fold into __other__
	if overflow == nil {
		t.Fatal("overflow returned nil")
	}
	m.GetTarget("d") // also folds into __other__
	overflow.RecordDial(time.Millisecond, true)

	snap := overflow.Snapshot()
	if snap.Host != targetOtherKey {
		t.Errorf("overflow Host=%q, want %q", snap.Host, targetOtherKey)
	}
	// "d" shares the __other__ bucket, so Dials >= 1
	if snap.Dials < 1 {
		t.Errorf("overflow Dials=%d, want >= 1", snap.Dials)
	}
}

func TestPerTargetCapDisabled(t *testing.T) {
	m := &Metrics{targetCap: 0}
	if m.GetTarget("x") != nil {
		t.Error("cap=0 should short-circuit to nil")
	}
	// SnapshotTargets with limit=10 returns nil per contract when the
	// underlying map is empty (which it always is when cap=0).
	if got := m.SnapshotTargets(10); len(got) != 0 {
		t.Errorf("SnapshotTargets with cap=0 returned %v, want empty", got)
	}
}

func TestSnapshotTargetsSortByBytesDown(t *testing.T) {
	m := &Metrics{targetCap: 8}
	for _, h := range []string{"small", "medium", "big"} {
		m.GetTarget(h)
	}
	m.GetTarget("small").RecordDown(10)
	m.GetTarget("medium").RecordDown(100)
	m.GetTarget("big").RecordDown(1000)

	top := m.SnapshotTargets(2)
	if len(top) != 2 {
		t.Fatalf("len(top)=%d, want 2", len(top))
	}
	if top[0].Host != "big" || top[1].Host != "medium" {
		t.Errorf("top=%+v, want big then medium", top)
	}
}

// ---- Prometheus exporter (moved to api/httpapi/prometheus_test.go) ----

// ---- TLS resumption counter ----

func TestRecordTLSResumed(t *testing.T) {
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
	m.RecordHandshake(HandshakeTLS, true, 10*time.Millisecond)
	m.RecordTLSResumed()
	m.RecordTLSResumed()
	snap := m.Snapshot()
	if snap.TLSHandshakeTotal != 1 {
		t.Errorf("TLSHandshakeTotal=%d, want 1", snap.TLSHandshakeTotal)
	}
	if snap.TLSHandshakeResumedTotal != 2 {
		t.Errorf("TLSHandshakeResumedTotal=%d, want 2", snap.TLSHandshakeResumedTotal)
	}
}
