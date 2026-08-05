package cluster

import (
	"net"
	"sync"
	"time"
)

// dnsCacheTTL bounds how long a resolved address is reused before the
// cluster code re-resolves it. Short on purpose: routing decisions must
// follow DNS changes (failover, geo steering) quickly, while still
// collapsing the repeated per-connection lookups the hot path performs.
const dnsCacheTTL = 30 * time.Second

type dnsCacheEntry struct {
	ip     string
	expiry time.Time
}

// dnsCache is a process-wide, mutex-guarded cache of successful DNS
// resolutions shared by the router, the matcher and the emergency
// fallback path, so a domain target is resolved once per TTL instead of
// once per layer. Only positive results are cached; failures fall through
// to the resolver every time.
var dnsCache = struct {
	mu      sync.Mutex
	entries map[string]dnsCacheEntry
}{entries: make(map[string]dnsCacheEntry)}

// resolveTargetIP resolves host to an IP string, reusing a cached result
// while fresh. Returns "" when resolution fails.
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

	resolved, err := net.ResolveIPAddr("ip", host)
	if err != nil || resolved.IP == nil {
		return ""
	}
	ip := resolved.IP.String()

	dnsCache.mu.Lock()
	dnsCache.entries[host] = dnsCacheEntry{ip: ip, expiry: now.Add(dnsCacheTTL)}
	dnsCache.mu.Unlock()
	return ip
}
