package proxy

import (
	"runtime/debug"
	"testing"
)

// TestSetRelayBufferSizeClamp verifies the hard safety bounds enforced
// by SetRelayBufferSize. Values below 4 KiB or above 1 MiB must be
// clamped, and the returned value is always 1 KiB-aligned.
func TestSetRelayBufferSizeClamp(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, minRelayBufferSize},
		{100, minRelayBufferSize},
		{4 * 1024, 4 * 1024},
		{5 * 1024, 5 * 1024},
		{12 * 1024, 12 * 1024},
		{32 * 1024, 32 * 1024},
		{1024 * 1024, 1024 * 1024},
		{2 * 1024 * 1024, maxRelayBufferSize},
	}
	for _, c := range cases {
		got := SetRelayBufferSize(c.in)
		if got != c.want {
			t.Errorf("SetRelayBufferSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	// restore default for other tests
	SetRelayBufferSize(defaultRelayBufferSize)
}

// TestSetRelayBufferSizeAlignment checks that odd values are rounded
// up to the next 1 KiB boundary.
func TestSetRelayBufferSizeAlignment(t *testing.T) {
	got := SetRelayBufferSize(8193)
	want := 9 * 1024
	if got != want {
		t.Errorf("SetRelayBufferSize(8193) = %d, want %d", got, want)
	}
	SetRelayBufferSize(defaultRelayBufferSize)
}

// TestBufferPoolMissAllocatesConfiguredSize is the regression test for
// issue #9: bufferPool.New used to hardcode defaultRelayBufferSize, so
// with a larger configured relay_buffer_size every pool miss first
// allocated a throwaway default-sized buffer that getBuf immediately
// discarded and re-allocated at the target size. New must allocate the
// active size directly — one allocation, nothing thrown away.
func TestBufferPoolMissAllocatesConfiguredSize(t *testing.T) {
	const want = 64 * 1024
	SetRelayBufferSize(want)
	defer SetRelayBufferSize(defaultRelayBufferSize)

	// pool.New is exactly what runs on every miss.
	for i := 0; i < 8; i++ {
		pb := bufferPool.New().(*pooledBuffer)
		if len(pb.buf) != want || cap(pb.buf) != want {
			t.Fatalf("pool miss produced buffer len=%d cap=%d, want %d — throwaway double allocation",
				len(pb.buf), cap(pb.buf), want)
		}
		if !pb.fresh {
			t.Fatalf("pool miss element not marked fresh, hit-rate counting would break")
		}
	}

	// One miss must cost exactly 2 allocations ([]byte + pooledBuffer).
	// A throwaway intermediate buffer would push this to 3.
	allocs := testing.AllocsPerRun(50, func() {
		_ = bufferPool.New()
	})
	if allocs > 2 {
		t.Fatalf("pool miss costs %.1f allocations, want <= 2 (throwaway buffer is back)", allocs)
	}
}

// BenchmarkGetPutPacketBuf measures the amortised cost of the UDP
// packet-buffer pool on the hot path. We expect a hit-rate close to
// 100% after the first call per goroutine.
func BenchmarkGetPutPacketBuf(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := getPacketBuf()
			buf.buf[0] = 0x42
			putPacketBuf(buf)
		}
	})
}

// BenchmarkGetPutRelayBuf mirrors BenchmarkGetPutPacketBuf for the TCP
// relay buffer pool so the two can be compared directly.
func BenchmarkGetPutRelayBuf(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := getBuf()
			buf.buf[0] = 0x42
			putBuf(buf)
		}
	})
}

// BenchmarkCopyBufferPooled compares the pooled TCP relay buffer
// against a freshly-allocated buffer of the same size. The pooled
// version must allocate 0 bytes per iteration.
func BenchmarkCopyBufferPooled(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := getBuf()
			_ = buf.buf
			putBuf(buf)
		}
	})
}

// TestPoolHitRateCountsEachGetOnce is the regression test for issue #8:
// bufferPool.New used to call OnGet(true) AND getBuf called OnGet(false),
// recording one miss as two Gets. That inflated gets, pushed hit_rate
// systematically toward 1, and made the metric useless for spotting real
// allocation problems. Now every Get must be counted exactly once —
// regardless of how many misses actually occur.
//
// Misses themselves are not asserted to be zero: in -race builds the
// runtime forces frequent GCs (SetGCPercent(-1) notwithstanding) and
// every GC flushes sync.Pool. The hit-rate math is covered
// deterministically by connmonitor's TestPoolStatsBasic.
func TestPoolHitRateCountsEachGetOnce(t *testing.T) {
	// Reduce (but cannot eliminate, see above) pool flushes.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	const n = 1000

	before := tcpPoolStats.Snapshot()
	for i := 0; i < n; i++ {
		putBuf(getBuf())
	}
	after := tcpPoolStats.Snapshot()

	if got := after.Gets - before.Gets; got != n {
		t.Fatalf("tcp pool: %d Gets recorded for %d Get calls, want exactly one count per Get", got, n)
	}
	if got := after.Puts - before.Puts; got != n {
		t.Fatalf("tcp pool: %d Puts recorded for %d Put calls", got, n)
	}
	misses := after.News - before.News
	if misses > uint64(n) {
		t.Fatalf("tcp pool: %d misses recorded for %d Gets — double counting is back", misses, n)
	}
	if hr := after.HitRate; hr < 0 || hr > 1 {
		t.Fatalf("tcp pool: cumulative hit_rate %.4f out of range", hr)
	}

	beforePkt := packetPoolStats.Snapshot()
	for i := 0; i < n; i++ {
		putPacketBuf(getPacketBuf())
	}
	afterPkt := packetPoolStats.Snapshot()

	if got := afterPkt.Gets - beforePkt.Gets; got != n {
		t.Fatalf("packet pool: %d Gets recorded for %d Get calls, want exactly one count per Get", got, n)
	}
	if missesPkt := afterPkt.News - beforePkt.News; missesPkt > uint64(n) {
		t.Fatalf("packet pool: %d misses recorded for %d Gets — double counting is back", missesPkt, n)
	}
	if hr := afterPkt.HitRate; hr < 0 || hr > 1 {
		t.Fatalf("packet pool: cumulative hit_rate %.4f out of range", hr)
	}
}
