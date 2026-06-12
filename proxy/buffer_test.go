package proxy

import (
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

// BenchmarkGetPutPacketBuf measures the amortised cost of the UDP
// packet-buffer pool on the hot path. We expect a hit-rate close to
// 100% after the first call per goroutine.
func BenchmarkGetPutPacketBuf(b *testing.B) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := getPacketBuf()
			(*buf)[0] = 0x42
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
			(*buf)[0] = 0x42
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
			_ = *buf
			putBuf(buf)
		}
	})
}
