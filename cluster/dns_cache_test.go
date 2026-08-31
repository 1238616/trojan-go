package cluster

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// swapResolver replaces resolveIPFunc for the duration of a test and
// restores the real resolver afterwards. The fake counts invocations so
// tests can assert how often the decision path actually hit a resolver.
func swapResolver(t *testing.T, fake func(host string) (string, error)) *int64Helper {
	t.Helper()
	orig := resolveIPFunc
	var calls int64Helper
	resolveIPFunc = func(_ context.Context, host string) (string, error) {
		calls.add()
		return fake(host)
	}
	t.Cleanup(func() { resolveIPFunc = orig })
	return &calls
}

// int64Helper is a tiny atomic counter to keep test assertions race-free
// without importing sync/atomic types into every call site.
type int64Helper struct {
	mu sync.Mutex
	n  int64
}

func (h *int64Helper) add() {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
}

func (h *int64Helper) load() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func TestResolveTargetIPCacheHit(t *testing.T) {
	calls := swapResolver(t, func(host string) (string, error) {
		return "192.0.2.10", nil
	})

	for i := 0; i < 5; i++ {
		if got := resolveTargetIP("cache-hit.test"); got != "192.0.2.10" {
			t.Fatalf("call %d: expected cached IP, got %q", i, got)
		}
	}
	if n := calls.load(); n != 1 {
		t.Fatalf("expected exactly 1 resolver call for 5 lookups, got %d", n)
	}
}

func TestResolveTargetIPAlreadyAnIP(t *testing.T) {
	calls := swapResolver(t, func(host string) (string, error) {
		t.Fatalf("resolver must not be called for IP input")
		return "", nil
	})
	if got := resolveTargetIP("192.0.2.99"); got != "192.0.2.99" {
		t.Fatalf("expected passthrough, got %q", got)
	}
	if n := calls.load(); n != 0 {
		t.Fatalf("expected 0 resolver calls, got %d", n)
	}
}

func TestResolveTargetIPFailureNotCached(t *testing.T) {
	var fail bool = true
	calls := swapResolver(t, func(host string) (string, error) {
		if fail {
			return "", fmt.Errorf("nxdomain")
		}
		return "192.0.2.20", nil
	})

	if got := resolveTargetIP("flaky.test"); got != "" {
		t.Fatalf("expected empty result on failure, got %q", got)
	}
	fail = false
	if got := resolveTargetIP("flaky.test"); got != "192.0.2.20" {
		t.Fatalf("expected resolution after recovery, got %q", got)
	}
	if n := calls.load(); n != 2 {
		t.Fatalf("expected 2 resolver calls (failure not cached), got %d", n)
	}
}

func TestResolveTargetIPSingleFlight(t *testing.T) {
	release := make(chan struct{})
	calls := swapResolver(t, func(host string) (string, error) {
		<-release // block until all callers are queued
		return "192.0.2.30", nil
	})

	const callers = 32
	results := make([]string, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = resolveTargetIP("single-flight.test")
		}(i)
	}
	// Give the goroutines time to queue on the in-flight entry.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, r := range results {
		if r != "192.0.2.30" {
			t.Fatalf("caller %d got %q, want 192.0.2.30", i, r)
		}
	}
	if n := calls.load(); n != 1 {
		t.Fatalf("expected 1 coalesced resolver call for %d callers, got %d", callers, n)
	}
}
