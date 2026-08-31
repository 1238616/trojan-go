package connmonitor

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkCalcTick drives the per-second sampling pass directly. The
// per-tick allocation count must not grow with the number of active
// connections (issue #8): sample slices are reused across ticks and
// reservoir inserts are batched under one lock per reservoir. Compare
// the allocs/op between conns=100 and conns=10000 — they should be
// nearly identical even though bytes/op grows with the connection count.
func BenchmarkCalcTick(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("conns=%d", n), func(b *testing.B) {
			m := NewMonitor()
			m.Stop() // tick is driven manually
			entries := make([]*connEntry, 0, n)
			for i := 0; i < n; i++ {
				entries = append(entries, m.RegisterEntry(fmt.Sprintf("bench-%d-%d", n, i), "bench:443"))
			}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Simulate traffic between ticks: atomic counter bumps,
				// zero allocations.
				for _, e := range entries {
					e.AddUpload(1500)
					e.AddDownload(6000)
				}
				m.tick(now)
			}
			b.StopTimer()
			for _, e := range entries {
				m.UnregisterEntry(e)
			}
		})
	}
}
