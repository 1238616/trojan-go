package cluster

import (
	"math"
	"testing"
	"time"
)

func TestRouteTableUpdate(t *testing.T) {
	rt := NewRouteTable("local", 50)
	rt.Update("149.154.175.53:443", "tokyo-1", 80*time.Millisecond)

	entries := rt.GetAllEntries()
	peers, ok := entries["149.154.175.53:443"]
	if !ok {
		t.Fatal("expected entry for 149.154.175.53:443")
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	if peers[0].PeerName != "tokyo-1" {
		t.Fatalf("expected tokyo-1, got %q", peers[0].PeerName)
	}
	if peers[0].RTT != 80*time.Millisecond {
		t.Fatalf("expected 80ms RTT, got %v", peers[0].RTT)
	}
	if !peers[0].Available {
		t.Fatal("expected available=true")
	}
}

func TestRouteTableEWMA(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// First update: RTT should be exactly the value (no smoothing)
	rt.Update("target:443", "peer-a", 100*time.Millisecond)

	// Second update with different value
	rt.Update("target:443", "peer-a", 200*time.Millisecond)

	entries := rt.GetAllEntries()
	peers := entries["target:443"]
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}

	// EWMA: 0.3 * 200 + 0.7 * 100 = 60 + 70 = 130ms
	expected := 130 * time.Millisecond
	tolerance := 2 * time.Millisecond
	diff := peers[0].RTT - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > tolerance {
		t.Fatalf("expected EWMA ~130ms, got %v", peers[0].RTT)
	}

	// Raw RTT should always be the latest
	if peers[0].RawRTT != 200*time.Millisecond {
		t.Fatalf("expected RawRTT=200ms, got %v", peers[0].RawRTT)
	}
}

func TestRouteTableBestExitBasic(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("149.154.175.53:443", "local", 250*time.Millisecond)
	rt.Update("149.154.175.53:443", "la-1", 15*time.Millisecond)
	rt.Update("149.154.175.53:443", "tokyo-1", 85*time.Millisecond)

	peer, gain := rt.BestExit("149.154.175.53", 443)
	if peer != "la-1" {
		t.Fatalf("expected la-1, got %q", peer)
	}
	expectedGain := 235 * time.Millisecond // 250 - 15
	if gain != expectedGain {
		t.Fatalf("expected gain=%v, got %v", expectedGain, gain)
	}
}

func TestRouteTableBestExitBelowThreshold(t *testing.T) {
	rt := NewRouteTable("local", 50) // threshold = 50ms

	rt.Update("target:443", "local", 80*time.Millisecond)
	rt.Update("target:443", "peer-a", 60*time.Millisecond)

	// Gain = 80 - 60 = 20ms < threshold 50ms → no relay
	peer, _ := rt.BestExit("target", 443)
	if peer != "" {
		t.Fatalf("expected empty (below threshold), got %q", peer)
	}
}

func TestRouteTableBestExitUnavailable(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("target:443", "local", 250*time.Millisecond)
	rt.Update("target:443", "peer-a", -1) // unavailable

	peer, _ := rt.BestExit("target", 443)
	if peer != "" {
		t.Fatalf("expected empty (peer unavailable), got %q", peer)
	}
}

func TestRouteTableStaleEntry(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("target:443", "local", 250*time.Millisecond)
	rt.Update("target:443", "peer-a", 10*time.Millisecond)

	// Manually make peer-a stale
	rt.mu.Lock()
	peers := rt.entries["target:443"]
	for i := range peers {
		if peers[i].PeerName == "peer-a" {
			peers[i].UpdatedAt = time.Now().Add(-15 * time.Minute)
		}
	}
	rt.mu.Unlock()

	peer, _ := rt.BestExit("target", 443)
	if peer != "" {
		t.Fatalf("expected empty (stale entry), got %q", peer)
	}
}

func TestRouteTableMultiplePeers(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("target:443", "local", 300*time.Millisecond)
	rt.Update("target:443", "peer-a", 100*time.Millisecond)
	rt.Update("target:443", "peer-b", 50*time.Millisecond)
	rt.Update("target:443", "peer-c", 200*time.Millisecond)

	peer, gain := rt.BestExit("target", 443)
	if peer != "peer-b" {
		t.Fatalf("expected peer-b (lowest), got %q", peer)
	}
	expectedGain := 250 * time.Millisecond // 300 - 50
	if gain != expectedGain {
		t.Fatalf("expected gain=%v, got %v", expectedGain, gain)
	}
}

func BenchmarkRouteTableBestExit(b *testing.B) {
	rt := NewRouteTable("local", 50)
	rt.Update("149.154.175.53:443", "local", 250*time.Millisecond)
	rt.Update("149.154.175.53:443", "la-1", 15*time.Millisecond)
	rt.Update("149.154.175.53:443", "tokyo-1", 85*time.Millisecond)
	rt.Update("149.154.175.53:443", "frankfurt-1", 120*time.Millisecond)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.BestExit("149.154.175.53", 443)
	}
}

func TestRouteTableNoLocalEntry(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Only peer data, no local
	rt.Update("target:443", "peer-a", 50*time.Millisecond)

	peer, _ := rt.BestExit("target", 443)
	if peer != "" {
		t.Fatalf("expected empty (no local baseline), got %q", peer)
	}
}

func TestRouteTableGetAllEntries(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("target-a:443", "local", 100*time.Millisecond)
	rt.Update("target-a:443", "peer-1", 50*time.Millisecond)
	rt.Update("target-b:80", "local", 200*time.Millisecond)

	entries := rt.GetAllEntries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(entries))
	}
	if len(entries["target-a:443"]) != 2 {
		t.Fatalf("expected 2 peers for target-a, got %d", len(entries["target-a:443"]))
	}

	// Verify it's a copy (modifying shouldn't affect original)
	entries["target-a:443"][0].RTT = time.Duration(math.MaxInt64)
	origEntries := rt.GetAllEntries()
	if origEntries["target-a:443"][0].RTT == time.Duration(math.MaxInt64) {
		t.Fatal("GetAllEntries should return a copy")
	}
}

func TestRouteTableBestExitLocalUnreachable(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Local probe failed (timeout) — returns -1 → stored as Available=false, RTT=0
	rt.Update("149.154.175.53:443", "local", -1)
	// Peer can reach the target
	rt.Update("149.154.175.53:443", "peer-jp", 80*time.Millisecond)

	peer, gain := rt.BestExit("149.154.175.53", 443)
	if peer != "peer-jp" {
		t.Fatalf("expected peer-jp (local unreachable, peer reachable), got %q", peer)
	}
	if gain != unreachableGain {
		t.Fatalf("expected unreachableGain=%v, got %v", unreachableGain, gain)
	}
}

func TestRouteTableBestExitLocalUnreachableMultiplePeers(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Local unreachable
	rt.Update("target:443", "local", -1)
	// Multiple peers: should pick fastest
	rt.Update("target:443", "peer-a", 200*time.Millisecond)
	rt.Update("target:443", "peer-b", 50*time.Millisecond)
	rt.Update("target:443", "peer-c", -1) // this peer also can't reach

	peer, gain := rt.BestExit("target", 443)
	if peer != "peer-b" {
		t.Fatalf("expected peer-b (fastest reachable), got %q", peer)
	}
	if gain != unreachableGain {
		t.Fatalf("expected unreachableGain, got %v", gain)
	}
}

func TestRouteTableBestExitLocalAndAllPeersUnreachable(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Everyone is down
	rt.Update("target:443", "local", -1)
	rt.Update("target:443", "peer-a", -1)

	peer, _ := rt.BestExit("target", 443)
	if peer != "" {
		t.Fatalf("expected empty (all unreachable), got %q", peer)
	}
}

func TestRouteTableEWMAResetAfterFailure(t *testing.T) {
	rt := NewRouteTable("local", 50)

	// Establish a baseline EWMA
	rt.Update("target:443", "peer-a", 100*time.Millisecond)
	rt.Update("target:443", "peer-a", 200*time.Millisecond)

	// Probe failure resets RTT to 0
	rt.Update("target:443", "peer-a", -1)
	entries := rt.GetAllEntries()
	if entries["target:443"][0].RTT != 0 {
		t.Fatalf("expected RTT=0 after failure, got %v", entries["target:443"][0].RTT)
	}

	// Recovery: should use raw value, not smooth against stale EWMA
	rt.Update("target:443", "peer-a", 50*time.Millisecond)
	entries = rt.GetAllEntries()
	if entries["target:443"][0].RTT != 50*time.Millisecond {
		t.Fatalf("expected RTT=50ms after recovery, got %v", entries["target:443"][0].RTT)
	}
	if !entries["target:443"][0].Available {
		t.Fatal("expected available=true after recovery")
	}
}

func TestRouteTablePrune(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("fresh:443", "local", 100*time.Millisecond)
	rt.Update("stale:443", "local", 200*time.Millisecond)

	// Make stale entry old
	rt.mu.Lock()
	peers := rt.entries["stale:443"]
	for i := range peers {
		peers[i].UpdatedAt = time.Now().Add(-15 * time.Minute)
	}
	rt.mu.Unlock()

	pruned := rt.Prune()
	if pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d", pruned)
	}

	entries := rt.GetAllEntries()
	if _, ok := entries["stale:443"]; ok {
		t.Fatal("stale entry should have been pruned")
	}
	if _, ok := entries["fresh:443"]; !ok {
		t.Fatal("fresh entry should still exist")
	}
}

func TestRouteTablePrunePartiallyFresh(t *testing.T) {
	rt := NewRouteTable("local", 50)

	rt.Update("target:443", "local", 100*time.Millisecond)
	rt.Update("target:443", "peer-a", 50*time.Millisecond)

	// Make only local stale, peer-a stays fresh
	rt.mu.Lock()
	peers := rt.entries["target:443"]
	for i := range peers {
		if peers[i].PeerName == "local" {
			peers[i].UpdatedAt = time.Now().Add(-15 * time.Minute)
		}
	}
	rt.mu.Unlock()

	pruned := rt.Prune()
	if pruned != 0 {
		t.Fatalf("expected 0 pruned (one peer still fresh), got %d", pruned)
	}
}
