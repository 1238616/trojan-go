package connmonitor

import (
	"sync"
	"sync/atomic"
)

// PoolStats tracks sync.Pool health for one named pool (e.g. "tcp" relay
// buffer pool, "packet" UDP buffer pool). It lets the dashboard surface
// hit-rate regressions before they become visible as GC pauses.
//
// All methods are safe for concurrent use; counters are atomic.
type PoolStats struct {
	name     string
	getTotal atomic.Uint64 // total Get() calls
	newTotal atomic.Uint64 // subset of Get() where the Pool's New() ran
	putTotal atomic.Uint64 // total Put() calls
}

// OnGet must be called once per Get(). isNew is true when the Pool's New
// function actually ran (i.e. the Get was a miss).
func (p *PoolStats) OnGet(isNew bool) {
	if p == nil {
		return
	}
	p.getTotal.Add(1)
	if isNew {
		p.newTotal.Add(1)
	}
}

// OnPut must be called once per Put().
func (p *PoolStats) OnPut() {
	if p == nil {
		return
	}
	p.putTotal.Add(1)
}

// PoolSample is the JSON-serialisable snapshot of one PoolStats.
type PoolSample struct {
	Name    string  `json:"name"`
	Gets    uint64  `json:"gets"`
	News    uint64  `json:"news"`
	Puts    uint64  `json:"puts"`
	HitRate float64 `json:"hit_rate"` // (gets - news) / gets; 0 when no gets
}

// Snapshot returns a point-in-time view of the pool counters.
func (p *PoolStats) Snapshot() PoolSample {
	if p == nil {
		return PoolSample{}
	}
	gets := p.getTotal.Load()
	news := p.newTotal.Load()
	var hit float64
	if gets > 0 {
		hit = float64(gets-news) / float64(gets)
		if hit < 0 {
			hit = 0
		}
	}
	return PoolSample{
		Name:    p.name,
		Gets:    gets,
		News:    news,
		Puts:    p.putTotal.Load(),
		HitRate: hit,
	}
}

// poolRegistry is the process-global list of registered pools. A sync.Map
// is used so RegisterPool is safe to call from init() in any package.
var poolRegistry sync.Map // name -> *PoolStats

// RegisterPool creates (or returns the existing) PoolStats for name.
// It is idempotent and typically called from a package-level var block.
func RegisterPool(name string) *PoolStats {
	if v, ok := poolRegistry.Load(name); ok {
		return v.(*PoolStats)
	}
	p := &PoolStats{name: name}
	actual, _ := poolRegistry.LoadOrStore(name, p)
	return actual.(*PoolStats)
}

// SnapshotPools returns a snapshot of every registered pool, in
// registration order is not guaranteed.
func SnapshotPools() []PoolSample {
	var out []PoolSample
	poolRegistry.Range(func(_, v interface{}) bool {
		out = append(out, v.(*PoolStats).Snapshot())
		return true
	})
	return out
}
