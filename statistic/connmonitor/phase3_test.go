package connmonitor

import (
	"testing"
)

// ---- Per-User metrics ----

func TestPerUserBasic(t *testing.T) {
	m := newTestMetrics()
	u := m.GetUser("hash_a")
	if u == nil {
		t.Fatal("GetUser returned nil")
	}
	u.RecordConn()
	u.RecordConn()
	u.RecordAuthFail()
	u.RecordUp(1000)
	u.RecordDown(5000)

	s := u.Snapshot()
	if s.Hash != "hash_a" {
		t.Errorf("Hash=%q want %q", s.Hash, "hash_a")
	}
	if s.Conns != 2 {
		t.Errorf("Conns=%d want 2", s.Conns)
	}
	if s.AuthFails != 1 {
		t.Errorf("AuthFails=%d want 1", s.AuthFails)
	}
	if s.BytesUp != 1000 {
		t.Errorf("BytesUp=%d want 1000", s.BytesUp)
	}
	if s.BytesDown != 5000 {
		t.Errorf("BytesDown=%d want 5000", s.BytesDown)
	}
}

func TestPerUserNilSafety(t *testing.T) {
	var u *PerUserMetrics
	// All methods should be safe on nil.
	u.RecordConn()
	u.RecordAuthFail()
	u.RecordUp(100)
	u.RecordDown(200)
	s := u.Snapshot()
	if s.Hash != "" || s.Conns != 0 || s.BytesDown != 0 {
		t.Errorf("nil Snapshot should be zero, got %+v", s)
	}
}

func TestPerUserCapOverflow(t *testing.T) {
	m := newTestMetrics()
	m.SetUserCap(3)

	// Fill the cap.
	m.GetUser("h1").RecordConn()
	m.GetUser("h2").RecordConn()
	m.GetUser("h3").RecordConn()

	// Existing users should still resolve normally.
	if u := m.GetUser("h1"); u.hash != "h1" {
		t.Errorf("expected h1, got %s", u.hash)
	}

	// Overflow user gets the __other__ bucket.
	overflow := m.GetUser("h4")
	if overflow.hash != userOtherKey {
		t.Errorf("overflow hash=%q want %q", overflow.hash, userOtherKey)
	}
	overflow.RecordDown(42)

	// Second overflow user merges into the same __other__ bucket.
	overflow2 := m.GetUser("h5")
	overflow2.RecordDown(8)

	users := m.SnapshotUsers(100)
	found := false
	for _, u := range users {
		if u.Hash == userOtherKey {
			found = true
			if u.BytesDown != 50 {
				t.Errorf("__other__ BytesDown=%d want 50", u.BytesDown)
			}
		}
	}
	if !found {
		t.Error("__other__ bucket not found in SnapshotUsers")
	}
}

func TestSnapshotUsersSortedByBytesDown(t *testing.T) {
	m := newTestMetrics()
	m.GetUser("a").RecordDown(100)
	m.GetUser("b").RecordDown(500)
	m.GetUser("c").RecordDown(300)

	users := m.SnapshotUsers(2)
	if len(users) != 2 {
		t.Fatalf("len=%d want 2", len(users))
	}
	if users[0].Hash != "b" {
		t.Errorf("users[0].Hash=%q want b", users[0].Hash)
	}
	if users[1].Hash != "c" {
		t.Errorf("users[1].Hash=%q want c", users[1].Hash)
	}
}

// ---- Mux stream metrics ----

func TestMuxStreamMetrics(t *testing.T) {
	m := newTestMetrics()

	m.RecordMuxStreamOpen()
	m.RecordMuxStreamOpen()
	m.RecordMuxStreamOpen()
	m.RecordMuxStreamClose()

	active := m.muxStreamsActive.Load()
	if active != 2 {
		t.Errorf("muxStreamsActive=%d want 2", active)
	}
	total := m.muxStreamsTotal.Load()
	if total != 3 {
		t.Errorf("muxStreamsTotal=%d want 3", total)
	}

	// Observe per-conn samples.
	m.ObserveMuxStreamsPerConn(3)
	m.ObserveMuxStreamsPerConn(7)
	m.ObserveMuxStreamsPerConn(5)

	snap := m.Snapshot()
	if snap.MuxStreamsActive != 2 {
		t.Errorf("Snapshot MuxStreamsActive=%d want 2", snap.MuxStreamsActive)
	}
	if snap.MuxStreamsTotal != 3 {
		t.Errorf("Snapshot MuxStreamsTotal=%d want 3", snap.MuxStreamsTotal)
	}
	if snap.MuxStreamsPerConnP50 <= 0 {
		t.Errorf("MuxStreamsPerConnP50=%f expected > 0", snap.MuxStreamsPerConnP50)
	}
}

func TestMuxQueueDepth(t *testing.T) {
	m := newTestMetrics()
	m.ObserveMuxQueueDepth(0)
	m.ObserveMuxQueueDepth(5)
	m.ObserveMuxQueueDepth(10)
	snap := m.Snapshot()
	if snap.MuxQueueDepthP50 <= 0 {
		t.Errorf("MuxQueueDepthP50=%f expected > 0", snap.MuxQueueDepthP50)
	}
}

// ---- helpers ----

// newTestMetrics returns a fresh Metrics with all reservoirs initialised.
// It deliberately does NOT use the global singleton so tests are isolated.
func newTestMetrics() *Metrics {
	m := &Metrics{
		upBpsRes:             newReservoir(metricsHistogramSampleCap),
		downBpsRes:           newReservoir(metricsHistogramSampleCap),
		tlsHandshakeLatRes:   newReservoir(metricsHistogramSampleCap),
		originDialLatRes:     newReservoir(metricsHistogramSampleCap),
		ttfbLatRes:           newReservoir(metricsHistogramSampleCap),
		trojanAuthLatRes:     newReservoir(metricsHistogramSampleCap),
		trojanAuthFailKinds:  make(map[string]uint64, 8),
		packetPpsRes:         newReservoir(metricsHistogramSampleCap),
		packetBpsRes:         newReservoir(metricsHistogramSampleCap),
		dnsResolveLatRes:     newReservoir(metricsHistogramSampleCap),
		tcpDialLatRes:        newReservoir(metricsHistogramSampleCap),
		targetCap:            targetCapDefault,
		userCap:              userCapDefault,
		muxStreamsPerConnRes: newReservoir(metricsHistogramSampleCap),
		muxQueueDepthRes:     newReservoir(metricsHistogramSampleCap),
	}
	return m
}
