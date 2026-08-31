package connmonitor

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRegisterAndGetAll(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	m.Register("c2", "google.com:80")

	all := m.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(all))
	}
	if all[0].Status != "active" || all[1].Status != "active" {
		t.Fatal("expected both active")
	}
}

func TestUnregister(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	m.Unregister("c1")

	all := m.GetAll()
	if len(all) != 1 || all[0].Status != "closed" {
		t.Fatalf("expected 1 closed connection, got %v", all)
	}
}

func TestRecordBytes(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	m.RecordUpload("c1", 1024)
	m.RecordDownload("c1", 2048)
	m.RecordUpload("c1", 512)

	all := m.GetAll()
	if all[0].UploadBytes != 1536 {
		t.Fatalf("expected upload 1536, got %d", all[0].UploadBytes)
	}
	if all[0].DownloadBytes != 2048 {
		t.Fatalf("expected download 2048, got %d", all[0].DownloadBytes)
	}
}

func TestSpeedCalculation(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	m.RecordUpload("c1", 10000)
	m.RecordDownload("c1", 20000)

	time.Sleep(1500 * time.Millisecond)

	all := m.GetAll()
	if all[0].UploadSpeed <= 0 {
		t.Fatalf("expected positive upload speed, got %f", all[0].UploadSpeed)
	}
	if all[0].DownloadSpeed <= 0 {
		t.Fatalf("expected positive download speed, got %f", all[0].DownloadSpeed)
	}
}

func TestGetSummary(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	m.Register("c2", "google.com:80")
	m.RecordUpload("c1", 100)
	m.RecordDownload("c2", 200)

	s := m.GetSummary()
	if s.TotalConnections != 2 {
		t.Fatalf("expected total 2, got %d", s.TotalConnections)
	}
	if s.ActiveConnections != 2 {
		t.Fatalf("expected active 2, got %d", s.ActiveConnections)
	}
	if s.TotalUploadBytes != 100 {
		t.Fatalf("expected upload 100, got %d", s.TotalUploadBytes)
	}
	if s.TotalDownloadBytes != 200 {
		t.Fatalf("expected download 200, got %d", s.TotalDownloadBytes)
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); m.RecordUpload("c1", 10) }()
		go func() { defer wg.Done(); m.RecordDownload("c1", 20) }()
		go func() { defer wg.Done(); m.GetAll() }()
	}
	wg.Wait()

	all := m.GetAll()
	if all[0].UploadBytes != 1000 {
		t.Fatalf("expected upload 1000, got %d", all[0].UploadBytes)
	}
	if all[0].DownloadBytes != 2000 {
		t.Fatalf("expected download 2000, got %d", all[0].DownloadBytes)
	}
}

func TestGetHistoryEmpty(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	h := m.GetHistory()
	if len(h) != 0 {
		t.Fatalf("expected empty history, got %d points", len(h))
	}
}

func TestHistoryRecording(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	m.RecordUpload("c1", 5000)
	m.RecordDownload("c1", 10000)

	// Wait for at least 3 calcLoop ticks to accumulate history
	time.Sleep(3500 * time.Millisecond)

	h := m.GetHistory()
	if len(h) < 3 {
		t.Fatalf("expected at least 3 history points, got %d", len(h))
	}

	// First point should have non-zero speeds (bytes were recorded before first tick)
	foundNonZero := false
	for _, p := range h {
		if p.UploadSpeed > 0 || p.DownloadSpeed > 0 {
			foundNonZero = true
			break
		}
	}
	if !foundNonZero {
		t.Fatal("expected at least one history point with non-zero speed")
	}

	// All points should have timestamps
	for _, p := range h {
		if p.Timestamp == 0 {
			t.Fatal("history point has zero timestamp")
		}
	}

	// Points should have active_conns >= 1
	for _, p := range h {
		if p.ActiveConns < 1 {
			t.Fatalf("expected active_conns >= 1, got %d", p.ActiveConns)
		}
	}
}

func TestHistoryChronologicalOrder(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.Register("c1", "example.com:443")
	time.Sleep(3500 * time.Millisecond)

	h := m.GetHistory()
	for i := 1; i < len(h); i++ {
		if h[i].Timestamp < h[i-1].Timestamp {
			t.Fatalf("history not in chronological order at index %d: %d < %d",
				i, h[i].Timestamp, h[i-1].Timestamp)
		}
	}
}

func TestGlobalSingleton(t *testing.T) {
	g1 := Global()
	g2 := Global()
	if g1 != g2 {
		t.Fatal("Global() should return the same instance")
	}
}

// TestGetAllAllocationDoesNotGrowWithTotalCount is a regression test for
// issue #2. GetAll must size its pre-allocation after the CURRENT number of
// connections, not after totalCount — a cumulative counter that never
// decreases. Before the fix, a long-running process that had served many
// connections allocated a slice proportional to the lifetime connection
// count on every /api/connections request, even when only a handful of
// connections were alive.
func TestGetAllAllocationDoesNotGrowWithTotalCount(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	const total = 10000
	entries := make([]*connEntry, 0, total)
	for i := 0; i < total; i++ {
		entries = append(entries, m.RegisterEntry(fmt.Sprintf("conn-%d", i), "example.com:443"))
	}
	for _, e := range entries {
		m.UnregisterEntry(e)
		// Backdate the scheduled deletion so calcLoop reaps the entry on
		// its next tick instead of waiting the usual 10s grace period.
		e.deleteAfter.Store(time.Now().UnixNano() - 1)
	}

	// Wait for calcLoop to reap the closed entries.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.GetAll()) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if got := m.totalCount.Load(); got != int64(total) {
		t.Fatalf("expected totalCount %d, got %d", total, got)
	}
	all := m.GetAll()
	if len(all) != 0 {
		t.Fatalf("expected no tracked connections after reap, got %d", len(all))
	}
	if cap(all) > 100 {
		t.Fatalf("GetAll pre-allocation still grows with cumulative totalCount: cap=%d", cap(all))
	}
}
