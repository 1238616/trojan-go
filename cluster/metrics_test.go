package cluster

import (
	"sync"
	"testing"
	"time"
)

func TestMetricsRecordRelay(t *testing.T) {
	m := NewClusterMetrics()

	m.RecordRelay("la-1", 200*time.Millisecond)
	m.RecordRelay("la-1", 180*time.Millisecond)
	m.RecordRelay("tokyo-1", 50*time.Millisecond)

	snap := m.Snapshot()
	if snap.TotalRelays != 3 {
		t.Fatalf("expected TotalRelays=3, got %d", snap.TotalRelays)
	}

	// Avg gain: (200+180+50)/3 = 143.33ms
	expectedAvg := float64(200+180+50) / 3.0
	diff := snap.AvgGainMs - expectedAvg
	if diff < -1 || diff > 1 {
		t.Fatalf("expected AvgGainMs~%.1f, got %.1f", expectedAvg, snap.AvgGainMs)
	}
}

func TestMetricsRecordFallback(t *testing.T) {
	m := NewClusterMetrics()

	m.RecordRelayFallback("la-1")
	m.RecordRelayFallback("la-1")
	m.RecordRelayFallback("tokyo-1")

	snap := m.Snapshot()
	if snap.TotalFallbacks != 3 {
		t.Fatalf("expected TotalFallbacks=3, got %d", snap.TotalFallbacks)
	}
}

func TestMetricsRecordProbe(t *testing.T) {
	m := NewClusterMetrics()

	m.RecordProbe(312 * time.Millisecond)

	snap := m.Snapshot()
	if snap.LastProbeAt == "" {
		t.Fatal("expected LastProbeAt to be set")
	}
	if snap.LastProbeDurMs < 311 || snap.LastProbeDurMs > 313 {
		t.Fatalf("expected LastProbeDurMs~312, got %.1f", snap.LastProbeDurMs)
	}
}

func TestMetricsConcurrency(t *testing.T) {
	m := NewClusterMetrics()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			m.RecordRelay("peer-1", 100*time.Millisecond)
		}()
		go func() {
			defer wg.Done()
			m.RecordRelayFallback("peer-1")
		}()
		go func() {
			defer wg.Done()
			_ = m.Snapshot()
		}()
	}
	wg.Wait()

	snap := m.Snapshot()
	if snap.TotalRelays != 100 {
		t.Fatalf("expected TotalRelays=100, got %d", snap.TotalRelays)
	}
	if snap.TotalFallbacks != 100 {
		t.Fatalf("expected TotalFallbacks=100, got %d", snap.TotalFallbacks)
	}
}

func TestMetricsZeroRelaysAvgGain(t *testing.T) {
	m := NewClusterMetrics()
	snap := m.Snapshot()
	if snap.AvgGainMs != 0 {
		t.Fatalf("expected AvgGainMs=0 with no relays, got %f", snap.AvgGainMs)
	}
	if snap.LastProbeAt != "" {
		t.Fatalf("expected empty LastProbeAt, got %q", snap.LastProbeAt)
	}
}

func BenchmarkMetricsRecordRelay(b *testing.B) {
	m := NewClusterMetrics()
	gain := 150 * time.Millisecond
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.RecordRelay("la-1", gain)
	}
}
