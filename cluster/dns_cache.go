package cluster

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// dnsCacheTTL bounds how long a resolved address is reused before the
	// cluster code re-resolves it. Short on purpose: routing decisions must
	// follow DNS changes (failover, geo steering) quickly, while still
	// collapsing the repeated per-connection lookups the hot path performs.
	dnsCacheTTL = 30 * time.Second

	// dnsResolveTimeout bounds a single resolution on the cluster decision
	// path. A hung resolver must not wedge relays; when it fires the
	// cluster layer degrades to local direct dial (issue #4).
	dnsResolveTimeout = 3 * time.Second
)

type dnsCacheEntry struct {
	ip     string
	expiry time.Time
}

// inflightCall is one in-progress resolution. Waiters block on done and
// then read ip; closing done broadcasts the result to every waiter.
type inflightCall struct {
	done chan struct{}
	ip   string
}

// dnsCache is a process-wide cache of successful DNS resolutions shared by
// the router, the matcher and the emergency fallback path, so a domain
// target is resolved once per TTL instead of once per layer and once per
// connection. Only positive results are cached; failures fall through to
// the resolver every time so recovery is immediate.
//
// Concurrent lookups for the same cold host are coalesced (single-flight):
// only the first caller performs the resolution, the rest wait for its
// result, so a burst of connections to a freshly blocked domain cannot
// stampede the resolver.
var dnsCache = struct {
	mu       sync.Mutex
	entries  map[string]dnsCacheEntry
	inflight map[string]*inflightCall
}{
	entries:  make(map[string]dnsCacheEntry),
	inflight: make(map[string]*inflightCall),
}

// dnsResolveCount counts the actual resolver round-trips performed by
// resolveTargetIP. Exposed for tests and for reasoning about the
// "at most one resolution per connection" guarantee (issue #4).
var dnsResolveCount atomic.Int64

// resolveIPFunc performs the actual resolution. Swappable in tests so the
// cache/single-flight behavior can be verified without touching the
// network. Returns an IP string or an error.
var resolveIPFunc = func(ctx context.Context, host string) (string, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	// Prefer the first IPv4 answer when available; the cluster decision
	// path only needs one usable address and IPv4 keeps route-table keys
	// in their canonical short form.
	for _, ipAddr := range ips {
		if ip4 := ipAddr.IP.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	if len(ips) == 0 {
		return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips[0].IP.String(), nil
}

// resolveTargetIP resolves host to an IP string, reusing a cached result
// while fresh and coalescing concurrent resolutions of the same host.
// Returns "" when resolution fails; callers must treat that as "degrade
// to local direct dial", never as a fatal error.
func resolveTargetIP(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return host
	}

	now := time.Now()

	dnsCache.mu.Lock()
	if e, ok := dnsCache.entries[host]; ok && now.Before(e.expiry) {
		dnsCache.mu.Unlock()
		return e.ip
	}
	if call, ok := dnsCache.inflight[host]; ok {
		// Another goroutine is already resolving this host — reuse its
		// result instead of issuing a duplicate query.
		dnsCache.mu.Unlock()
		select {
		case <-call.done:
			return call.ip
		case <-time.After(dnsResolveTimeout + time.Second):
			// The owner goroutine is stuck; don't block the data path on it.
			return ""
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	dnsCache.inflight[host] = call
	// Opportunistic sweep so the map doesn't grow unboundedly with
	// one-off hosts. Cheap: only walks the map when something expired.
	if len(dnsCache.entries) > 64 {
		for k, e := range dnsCache.entries {
			if now.After(e.expiry) {
				delete(dnsCache.entries, k)
			}
		}
	}
	dnsCache.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), dnsResolveTimeout)
	defer cancel()
	dnsResolveCount.Add(1)
	resolved, err := resolveIPFunc(ctx, host)

	dnsCache.mu.Lock()
	delete(dnsCache.inflight, host)
	if err == nil && resolved != "" {
		dnsCache.entries[host] = dnsCacheEntry{ip: resolved, expiry: time.Now().Add(dnsCacheTTL)}
	}
	dnsCache.mu.Unlock()

	// Publish to every waiter at once.
	call.ip = resolved
	close(call.done)
	return resolved
}
