package freedom

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

// PoolConfig holds the knobs exposed to operators. It mirrors the
// design-plan's UpstreamPoolConfig but is re-declared here so the
// freedom package has no dependency on proxy.Config.
type PoolConfig struct {
	MaxIdlePerHost int
	IdleTTL        time.Duration
	HealthProbe    time.Duration
	Enabled        bool
}

// DefaultPoolConfig is what ConnPool uses when caller passes the zero
// value. The defaults are deliberately conservative so the pool can
// be enabled by default without destabilising existing deployments.
var DefaultPoolConfig = PoolConfig{
	MaxIdlePerHost: 8,
	IdleTTL:        60 * time.Second,
	HealthProbe:    15 * time.Second,
	Enabled:        false,
}

// poolStats is the process-global PoolStats sink for the upstream
// connection pool. The dashboard's "Pool & UDP" card renders this.
var poolStats = connmonitor.RegisterPool("upstream_conn")

// pooledConn wraps a net.Conn with bookkeeping fields.
type pooledConn struct {
	net.Conn
	putAt time.Time
}

// bucket holds idle connections for one target address. LIFO: Get pops
// from the end of idle, Put appends. LIFO is preferred over FIFO
// because the most-recently-used connection is the least likely to have
// been closed by the peer, and it stays warm in the kernel's TCP cache.
type bucket struct {
	mu      sync.Mutex
	idle    []*pooledConn
	maxIdle int
}

// ConnPool is a per-host LIFO connection pool. It is safe for
// concurrent use and designed to run for the lifetime of the process.
// The zero value is not usable; use NewConnPool.
type ConnPool struct {
	cfg     PoolConfig
	perHost sync.Map // addr -> *bucket
	stopCh  chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

// NewConnPool constructs a ConnPool and starts the background reaper.
// If cfg.Enabled is false, the reaper is not started and Get/Put
// short-circuit.
func NewConnPool(cfg PoolConfig) *ConnPool {
	if cfg.MaxIdlePerHost <= 0 {
		cfg.MaxIdlePerHost = DefaultPoolConfig.MaxIdlePerHost
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = DefaultPoolConfig.IdleTTL
	}
	if cfg.HealthProbe <= 0 {
		cfg.HealthProbe = DefaultPoolConfig.HealthProbe
	}
	p := &ConnPool{
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
	if cfg.Enabled {
		go p.reapLoop()
	}
	return p
}

// Close shuts down the pool and closes every idle connection.
func (p *ConnPool) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	p.once.Do(func() { close(p.stopCh) })
	p.perHost.Range(func(_, v interface{}) bool {
		b := v.(*bucket)
		b.mu.Lock()
		for _, c := range b.idle {
			_ = c.Conn.Close()
		}
		b.idle = nil
		b.mu.Unlock()
		return true
	})
}

// Get borrows an idle connection for addr. ok == false means no usable
// idle conn; the caller must dial a fresh one. When ok == true the
// caller owns the conn and is responsible for either Put()-ing it back
// or Close()-ing it.
func (p *ConnPool) Get(_ context.Context, addr string) (net.Conn, bool) {
	if p == nil || !p.cfg.Enabled || p.closed.Load() {
		return nil, false
	}
	v, ok := p.perHost.Load(addr)
	if !ok {
		return nil, false
	}
	b := v.(*bucket)
	b.mu.Lock()
	for len(b.idle) > 0 {
		last := len(b.idle) - 1
		c := b.idle[last]
		b.idle = b.idle[:last]
		b.mu.Unlock()

		// Liveness probe: issue a zero-timeout read. If the peer
		// closed the conn, Read returns an error other than
		// ErrDeadlineExceeded and we discard it.
		_ = c.Conn.SetReadDeadline(time.Now())
		probe := make([]byte, 1)
		_, err := c.Conn.Read(probe)
		if err == nil {
			// Peer sent data; this conn is tainted — drop it.
			_ = c.Conn.Close()
			b.mu.Lock()
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			// expected: no data available, conn still good
			_ = c.Conn.SetReadDeadline(time.Time{})
			poolStats.OnGet(false) // hit
			return c.Conn, true
		}
		// any other error -> peer closed / RST
		_ = c.Conn.Close()
		b.mu.Lock()
	}
	b.mu.Unlock()
	return nil, false
}

// Put returns a connection to the pool. If the bucket is at capacity
// or the pool is disabled, the conn is closed immediately.
func (p *ConnPool) Put(addr string, conn net.Conn) {
	if p == nil || !p.cfg.Enabled || p.closed.Load() || conn == nil {
		if conn != nil {
			_ = conn.Close()
		}
		return
	}
	v, _ := p.perHost.LoadOrStore(addr, &bucket{maxIdle: p.cfg.MaxIdlePerHost})
	b := v.(*bucket)

	pc := &pooledConn{Conn: conn, putAt: time.Now()}
	b.mu.Lock()
	if len(b.idle) >= b.maxIdle {
		b.mu.Unlock()
		_ = conn.Close()
		return
	}
	b.idle = append(b.idle, pc)
	b.mu.Unlock()
	poolStats.OnPut()
}

// reapLoop is the background goroutine that evicts idle connections
// past their TTL. It runs once per HealthProbe interval and closes
// expired conns in place.
func (p *ConnPool) reapLoop() {
	t := time.NewTicker(p.cfg.HealthProbe)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			now := time.Now()
			p.perHost.Range(func(_, v interface{}) bool {
				b := v.(*bucket)
				b.mu.Lock()
				kept := b.idle[:0]
				for _, c := range b.idle {
					if now.Sub(c.putAt) > p.cfg.IdleTTL {
						_ = c.Conn.Close()
						continue
					}
					kept = append(kept, c)
				}
				// overwrite to release references
				for i := len(kept); i < len(b.idle); i++ {
					b.idle[i] = nil
				}
				b.idle = kept
				b.mu.Unlock()
				return true
			})
		}
	}
}

// Len returns the total number of idle connections across all hosts.
// Used by tests and by the dashboard for a quick "pool depth" signal.
func (p *ConnPool) Len() int {
	if p == nil {
		return 0
	}
	total := 0
	p.perHost.Range(func(_, v interface{}) bool {
		b := v.(*bucket)
		b.mu.Lock()
		total += len(b.idle)
		b.mu.Unlock()
		return true
	})
	return total
}

// init ensures a harmless default pool exists even when no config is
// supplied. Call sites that load a real config will replace this via
// SetGlobalPool.
var (
	globalPool     *ConnPool
	globalPoolOnce sync.Once
)

// GlobalPool returns the process-global ConnPool singleton. Callers
// that want a non-default config should call SetGlobalPool during
// startup, before any DialConn invocations.
func GlobalPool() *ConnPool {
	globalPoolOnce.Do(func() {
		globalPool = NewConnPool(DefaultPoolConfig)
		log.Debugf("freedom: upstream conn pool created (enabled=%v)", DefaultPoolConfig.Enabled)
	})
	return globalPool
}

// SetGlobalPool replaces the process-global pool. The previous pool,
// if any, is closed. It is the caller's responsibility to invoke this
// during startup.
func SetGlobalPool(p *ConnPool) {
	if globalPool != nil {
		globalPool.Close()
	}
	globalPool = p
	// reset the once so GlobalPool won't re-create after explicit set
	globalPoolOnce = sync.Once{}
	globalPoolOnce.Do(func() {})
}
