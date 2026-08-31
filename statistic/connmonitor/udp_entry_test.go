package connmonitor

import (
	"fmt"
	"testing"
	"time"
)

// Tests for issue #10: UDP packet flows must be first-class citizens in
// the connection monitor (type distinction, byte accounting, and the
// packet_pps / packet_bps samplers).

func TestRegisterPacketEntryType(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	m.RegisterEntry("tcp-1", "example.com:443")
	m.RegisterPacketEntry("udp-1", "udp")

	all := m.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	types := map[string]string{}
	for _, ci := range all {
		types[ci.ID] = ci.Type
	}
	if types["tcp-1"] != "tcp" {
		t.Fatalf("tcp entry type = %q, want tcp", types["tcp-1"])
	}
	if types["udp-1"] != "udp" {
		t.Fatalf("udp entry type = %q, want udp", types["udp-1"])
	}
}

func TestSetTargetOnceFirstWins(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	e := m.RegisterPacketEntry("udp-1", "udp")
	if e.TargetKnown() {
		t.Fatal("fresh UDP entry must not report its target as known")
	}
	e.SetTargetOnce("8.8.8.8:53")
	e.SetTargetOnce("1.1.1.1:443") // must be ignored
	if !e.TargetKnown() {
		t.Fatal("entry must report target known after SetTargetOnce")
	}

	all := m.GetAll()
	if all[0].Target != "8.8.8.8:53" {
		t.Fatalf("target = %q, want 8.8.8.8:53 (first call wins)", all[0].Target)
	}
}

// TestUDPEntryFeedsPacketMetrics asserts that calcLoop routes UDP flow
// deltas into the packet_pps / packet_bps reservoirs, which is what makes
// the /api/metrics packet histograms non-zero under real traffic.
func TestUDPEntryFeedsPacketMetrics(t *testing.T) {
	m := NewMonitor()
	defer m.Stop()

	metrics := GlobalMetrics()
	id := fmt.Sprintf("udp-metrics-%d", time.Now().UnixNano())
	e := m.RegisterPacketEntry(id, "udp")
	e.AddPacket()
	e.AddPacket()
	e.AddPacket()
	e.AddPacket()
	e.AddPacket()
	e.AddUpload(120)

	// Wait for at least two calcLoop ticks so the packet/byte deltas are
	// observed regardless of ticker phase.
	time.Sleep(2300 * time.Millisecond)

	snap := metrics.Snapshot()
	if snap.PacketPpsP95 <= 0 {
		t.Fatalf("expected non-zero packet_pps after UDP traffic, p95=%f", snap.PacketPpsP95)
	}
	if snap.PacketBpsP95 <= 0 {
		t.Fatalf("expected non-zero packet_bps after UDP traffic, p95=%f", snap.PacketBpsP95)
	}

	m.UnregisterEntry(e)
}
