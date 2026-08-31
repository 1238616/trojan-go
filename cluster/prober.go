package cluster

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p4gefau1t/trojan-go/log"
)

// ProbeTarget is a destination to measure latency against.
type ProbeTarget struct {
	Host string
	Port int
	// CIDR holds the normalized CIDR string when this target represents a
	// static cidr: rule. Host then carries a representative address inside
	// the block that is dialed for the measurement, while route-table
	// entries are keyed by the CIDR itself so lookups for any IP inside
	// the block can use it (issue #5).
	CIDR string
}

func (pt ProbeTarget) Key() string {
	if pt.CIDR != "" {
		return pt.CIDR
	}
	return net.JoinHostPort(pt.Host, strconv.Itoa(pt.Port))
}

// cidrRepresentative returns the address probed on behalf of a CIDR target:
// the first host address in the block (network address + 1), or the network
// address itself when the block is a single host (/32, /128). Probing one
// representative is only an approximation of the whole block's latency, but
// it is what turns a CIDR rule from "matched but never measured" into an
// entry the route table can actually answer with.
func cidrRepresentative(ipnet *net.IPNet) net.IP {
	ip := ipnet.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	ones, bits := ipnet.Mask.Size()
	rep := make(net.IP, len(ip))
	copy(rep, ip)
	if ones >= bits {
		return rep
	}
	for i := len(rep) - 1; i >= 0; i-- {
		rep[i]++
		if rep[i] != 0 {
			break
		}
	}
	return rep
}

// targetRuleTypes are the recognized "type:value" prefixes in cluster
// target lists. Anything else is treated as a bare "host[:port]" target.
var targetRuleTypes = map[string]bool{
	"cidr":    true,
	"domain":  true,
	"ip":      true,
	"geoip":   true,
	"geosite": true,
}

// splitTargetRule splits a configured target into (ruleType, value);
// ruleType is "" for unprefixed "host[:port]" targets.
func splitTargetRule(t string) (ruleType, value string) {
	parts := strings.SplitN(t, ":", 2)
	if len(parts) == 2 {
		prefix := strings.ToLower(strings.TrimSpace(parts[0]))
		if targetRuleTypes[prefix] {
			return prefix, strings.TrimSpace(parts[1])
		}
	}
	return "", t
}

// DynamicTarget is registered by actual traffic when dial RTT exceeds
// threshold.
//
// Instances are immutable after publication: RegisterSlowTarget replaces
// the whole pointer in dynamicTargets instead of mutating fields in place,
// because registration runs concurrently from many proxy goroutines while
// Snapshot() and the eviction loop read the values.
type DynamicTarget struct {
	Host        string
	Port        int
	FirstSeenAt time.Time // when the target was first registered (never overwritten)
	LastSeenAt  time.Time // last registration; used for staleness eviction
	LastDialRTT time.Duration
}

const maxDynamicTargets = 50

// maxUrgentProbes bounds how many data-path-triggered urgent probes may run
// at once. A burst of newly blocked targets otherwise fans out into an
// unbounded number of full tunnel probes.
const maxUrgentProbes = 3

// peerAliveInfo is the cached result of the last connectivity check for a
// peer. Snapshot() reads this instead of re-dialing peers on every API
// request.
type peerAliveInfo struct {
	Alive     bool
	RTT       time.Duration
	CheckedAt time.Time
}

// Prober periodically measures latency from each peer (and local) to
// registered targets. It is fully asynchronous and never blocks data paths.
type Prober struct {
	ctx              context.Context
	cancel           context.CancelFunc
	peers            []PeerConfig
	peerDialers      map[string]*PeerDialer
	staticTargets    []ProbeTarget
	dynamicTargets   sync.Map // map[string]*DynamicTarget
	dynamicCount     int32    // atomic; number of entries in dynamicTargets
	routeTable       *RouteTable
	interval         time.Duration
	timeout          time.Duration
	latencyThreshold time.Duration
	localName        string
	metrics          *ClusterMetrics
	peerAlive        sync.Map // map[string]peerAliveInfo — peer connectivity status
	urgentSem        chan struct{}
}

func NewProber(ctx context.Context, cfg *Config, routeTable *RouteTable, peerDialers map[string]*PeerDialer, metrics *ClusterMetrics) *Prober {
	ctx, cancel := context.WithCancel(ctx)

	interval := time.Duration(cfg.ProbeInterval) * time.Second
	if interval < 60*time.Second {
		interval = 60 * time.Second
		log.Warnf("cluster: probe_interval clamped to minimum 60s (requested %ds)", cfg.ProbeInterval)
	}

	timeout := time.Duration(cfg.ProbeTimeout) * time.Millisecond
	if timeout == 0 {
		timeout = 3 * time.Second
	}

	latencyThreshold := time.Duration(cfg.LatencyThreshold) * time.Millisecond

	// Parse static targets from config. All rule shapes the matcher
	// understands must also become probe targets here, otherwise the
	// matcher lets a connection through to the route table but the table
	// never holds any measurement for it (issue #5).
	var staticTargets []ProbeTarget
	for _, t := range cfg.Targets {
		ruleType, value := splitTargetRule(t)
		switch ruleType {
		case "cidr":
			_, ipnet, err := net.ParseCIDR(value)
			if err != nil {
				log.Warnf("cluster: invalid cidr target %q, ignored", t)
				continue
			}
			// Probe a representative address of the block; the result is
			// published under the CIDR key (see ProbeTarget.Key).
			staticTargets = append(staticTargets, ProbeTarget{
				Host: cidrRepresentative(ipnet).String(),
				Port: 443,
				CIDR: ipnet.String(),
			})
		case "ip":
			if net.ParseIP(value) == nil {
				log.Warnf("cluster: invalid ip target %q, ignored", t)
				continue
			}
			staticTargets = append(staticTargets, ProbeTarget{Host: value, Port: 443})
		case "domain":
			staticTargets = append(staticTargets, ProbeTarget{Host: value, Port: 443})
		case "geoip", "geosite":
			log.Warnf("cluster: %s target %q is not supported for probing, ignored", ruleType, t)
		case "":
			// Unprefixed "host:port" or bare IP.
			host, portStr, err := net.SplitHostPort(value)
			if err != nil {
				if ip := net.ParseIP(value); ip != nil {
					staticTargets = append(staticTargets, ProbeTarget{Host: value, Port: 443})
				} else {
					log.Warnf("cluster: invalid target %q, ignored", t)
				}
				continue
			}
			port, _ := strconv.Atoi(portStr)
			if port > 0 {
				staticTargets = append(staticTargets, ProbeTarget{Host: host, Port: port})
			}
		default:
			log.Warnf("cluster: unknown target rule type %q, ignored", ruleType)
		}
	}

	return &Prober{
		ctx:              ctx,
		cancel:           cancel,
		peers:            cfg.Peers,
		peerDialers:      peerDialers,
		staticTargets:    staticTargets,
		routeTable:       routeTable,
		interval:         interval,
		timeout:          timeout,
		latencyThreshold: latencyThreshold,
		localName:        cfg.NodeName,
		metrics:          metrics,
		urgentSem:        make(chan struct{}, maxUrgentProbes),
	}
}

// Start launches the probe loop in background.
func (p *Prober) Start() {
	go p.probeLoop()
	go p.evictionLoop()
	log.Infof("cluster: prober started (interval=%s, threshold=%s, static_targets=%d)",
		p.interval, p.latencyThreshold, len(p.staticTargets))
}

func (p *Prober) Stop() {
	p.cancel()
}

// RegisterSlowTarget is called from the data path when origin dial RTT
// exceeds latencyThreshold, or when the dial failed outright. It is
// lock-free (sync.Map + atomic counter) and safe to call concurrently from
// hot paths.
//
// dialFailed distinguishes the two callers:
//   - failed dial (typically a timeout, i.e. the target looks blocked):
//     the route table is immediately updated to mark local as unreachable
//     so subsequent connections can relay through a peer without waiting
//     for the next probe cycle;
//   - slow but successful dial: the target is registered for probing, but
//     local stays marked reachable — slow != unreachable.
func (p *Prober) RegisterSlowTarget(host string, port int, dialRTT time.Duration, dialFailed bool) {
	if dialRTT < p.latencyThreshold {
		log.DebugKV("cluster: dial below threshold, skip",
			"host", host, "port", port,
			"dial_rtt_ms", dialRTT.Milliseconds(),
			"threshold_ms", p.latencyThreshold.Milliseconds())
		return
	}
	key := net.JoinHostPort(host, strconv.Itoa(port))
	now := time.Now()
	isNew := false

	if v, loaded := p.dynamicTargets.Load(key); loaded {
		// Existing target — publish an updated immutable copy.
		// FirstSeenAt is preserved from the original registration.
		old := v.(*DynamicTarget)
		updated := *old
		updated.LastSeenAt = now
		updated.LastDialRTT = dialRTT
		p.dynamicTargets.Store(key, &updated)
	} else {
		// New target — reserve a slot against the cap before inserting,
		// instead of inserting first and deleting afterwards.
		reserved := false
		for {
			cur := atomic.LoadInt32(&p.dynamicCount)
			if cur >= maxDynamicTargets {
				break
			}
			if atomic.CompareAndSwapInt32(&p.dynamicCount, cur, cur+1) {
				reserved = true
				break
			}
		}
		if !reserved {
			log.DebugKV("cluster: dynamic target cap reached, dropping",
				"host", host, "port", port,
				"max", maxDynamicTargets)
			return
		}
		if _, loaded := p.dynamicTargets.LoadOrStore(key, &DynamicTarget{
			Host:        host,
			Port:        port,
			FirstSeenAt: now,
			LastSeenAt:  now,
			LastDialRTT: dialRTT,
		}); loaded {
			// Lost a race with another goroutine — release the slot and
			// refresh the winner's copy instead.
			atomic.AddInt32(&p.dynamicCount, -1)
			if v, ok := p.dynamicTargets.Load(key); ok {
				old := v.(*DynamicTarget)
				updated := *old
				updated.LastSeenAt = now
				updated.LastDialRTT = dialRTT
				p.dynamicTargets.Store(key, &updated)
			}
		} else {
			isNew = true
		}
	}

	if dialFailed {
		// Local dial failed (timeout): mark local unreachable so the next
		// connection can relay through a peer immediately.
		p.routeTable.Update(key, p.localName, -1)
		log.InfoKV("cluster: local dial failed, local marked unreachable",
			"host", host, "port", port,
			"dial_rtt_ms", dialRTT.Milliseconds())
		if isNew {
			// Bound the urgent-probe fan-out: a burst of newly blocked
			// targets must not spawn an unbounded number of tunnel probes.
			select {
			case p.urgentSem <- struct{}{}:
				go func() {
					defer func() { <-p.urgentSem }()
					p.probeTargetAllPeers(ProbeTarget{Host: host, Port: port})
				}()
			default:
				log.DebugKV("cluster: urgent probe deferred (probe slots busy)",
					"host", host, "port", port)
			}
		}
		return
	}

	log.DebugKV("cluster: slow target registered",
		"host", host, "port", port,
		"dial_rtt_ms", dialRTT.Milliseconds(),
		"threshold_ms", p.latencyThreshold.Milliseconds())
}

// DynamicTargetCount returns the number of dynamically registered targets.
// O(1): backed by an atomic counter maintained on insert/delete.
func (p *Prober) DynamicTargetCount() int {
	return int(atomic.LoadInt32(&p.dynamicCount))
}

func (p *Prober) probeLoop() {
	// Initial probe after a short delay
	select {
	case <-time.After(5 * time.Second):
		p.probeAll()
	case <-p.ctx.Done():
		return
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.probeAll()
		}
	}
}

func (p *Prober) evictionLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.evictStaleTargets()
			if n := p.routeTable.Prune(); n > 0 {
				log.Debugf("cluster: pruned %d stale route table entries", n)
			}
		}
	}
}

// probeAll probes all targets (static + dynamic) across all peers and local.
func (p *Prober) probeAll() {
	// Always check peer connectivity first
	p.probePeerAlive()

	targets := make([]ProbeTarget, len(p.staticTargets))
	copy(targets, p.staticTargets)

	p.dynamicTargets.Range(func(key, value interface{}) bool {
		dt := value.(*DynamicTarget)
		targets = append(targets, ProbeTarget{Host: dt.Host, Port: dt.Port})
		return true
	})

	if len(targets) == 0 {
		log.Debug("cluster: no probe targets (static=0, dynamic=0), skipping target probes")
		return
	}

	start := time.Now()
	log.Debugf("cluster: probing %d targets across %d peers", len(targets), len(p.peers))

	// Peer probes go through full tunnel stack (TCP+TLS+WS+Trojan),
	// so limit concurrency to avoid starving normal mux traffic.
	const maxPeerProbes = 5
	sem := make(chan struct{}, maxPeerProbes)

	var wg sync.WaitGroup
	for _, target := range targets {
		target := target

		// Probe local (direct TCP connect) — lightweight, no limit needed
		wg.Add(1)
		go func() {
			defer wg.Done()
			rtt := p.probeDirect(target)
			p.routeTable.Update(target.Key(), p.localName, rtt)
			log.DebugKV("cluster: probe local",
				"target", target.Key(),
				"rtt_ms", rtt.Milliseconds())
		}()

		// Probe each peer (via trojan/websocket tunnel) — rate-limited
		for _, peer := range p.peers {
			peer := peer
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				dialer, ok := p.peerDialers[peer.Name]
				if !ok {
					return
				}
				rtt := dialer.ProbeWithTimeout(target, p.timeout)
				p.routeTable.Update(target.Key(), peer.Name, rtt)
				log.DebugKV("cluster: probe peer",
					"peer", peer.Name,
					"target", target.Key(),
					"rtt_ms", rtt.Milliseconds(),
					"available", rtt >= 0)
			}()
		}
	}
	wg.Wait()

	dur := time.Since(start)
	p.metrics.RecordProbe(dur)
	log.Infof("cluster: probe complete in %s", dur)
}

// probeDirect measures local TCP connect latency to target.
func (p *Prober) probeDirect(target ProbeTarget) time.Duration {
	addr := fmt.Sprintf("%s:%d", target.Host, target.Port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, p.timeout)
	if err != nil {
		log.DebugKV("cluster: local probe failed",
			"target", addr, "err", err)
		return -1
	}
	rtt := time.Since(start)
	conn.Close()
	return rtt
}

// probePeerAlive checks connectivity to each peer via TCP+TLS(+WS) handshake.
// Results (alive flag + RTT + timestamp) are cached so Snapshot() can serve
// the API without re-dialing every peer.
func (p *Prober) probePeerAlive() {
	var wg sync.WaitGroup
	for _, peer := range p.peers {
		peer := peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			info := peerAliveInfo{CheckedAt: time.Now()}
			dialer, ok := p.peerDialers[peer.Name]
			if !ok {
				p.peerAlive.Store(peer.Name, info)
				return
			}
			rtt := dialer.CheckAlive(p.timeout)
			info.RTT = rtt
			info.Alive = rtt >= 0
			p.peerAlive.Store(peer.Name, info)
			if info.Alive {
				log.DebugKV("cluster: peer alive", "peer", peer.Name, "rtt_ms", rtt.Milliseconds())
			} else {
				log.DebugKV("cluster: peer unreachable", "peer", peer.Name)
			}
		}()
	}
	wg.Wait()
}

// IsPeerAlive returns whether the peer passed its last connectivity check.
func (p *Prober) IsPeerAlive(peerName string) bool {
	info, ok := p.PeerAliveInfo(peerName)
	return ok && info.Alive
}

// PeerAliveInfo returns the cached result of the peer's last connectivity
// check.
func (p *Prober) PeerAliveInfo(peerName string) (peerAliveInfo, bool) {
	v, ok := p.peerAlive.Load(peerName)
	if !ok {
		return peerAliveInfo{}, false
	}
	info, isInfo := v.(peerAliveInfo)
	if !isInfo {
		// Legacy bool entry (tests or older code paths).
		if alive, isBool := v.(bool); isBool {
			return peerAliveInfo{Alive: alive}, true
		}
		return peerAliveInfo{}, false
	}
	return info, true
}

// probeTargetAllPeers probes a single target across all peers in parallel
// and updates the route table. Called asynchronously when a new blocked
// target is discovered so that BestExit can return a relay peer immediately.
func (p *Prober) probeTargetAllPeers(target ProbeTarget) {
	var wg sync.WaitGroup
	for _, peer := range p.peers {
		peer := peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialer, ok := p.peerDialers[peer.Name]
			if !ok {
				return
			}
			rtt := dialer.ProbeWithTimeout(target, p.timeout)
			p.routeTable.Update(target.Key(), peer.Name, rtt)
			log.DebugKV("cluster: urgent probe peer",
				"peer", peer.Name,
				"target", target.Key(),
				"rtt_ms", rtt.Milliseconds(),
				"available", rtt >= 0)
		}()
	}
	wg.Wait()
}

// evictStaleTargets removes dynamic targets not refreshed within 30 minutes.
func (p *Prober) evictStaleTargets() {
	cutoff := time.Now().Add(-30 * time.Minute)
	p.dynamicTargets.Range(func(key, value interface{}) bool {
		dt := value.(*DynamicTarget)
		if dt.LastSeenAt.Before(cutoff) {
			if _, loaded := p.dynamicTargets.LoadAndDelete(key); loaded {
				atomic.AddInt32(&p.dynamicCount, -1)
			}
			log.DebugKV("cluster: evicted stale probe target", "host", dt.Host)
		}
		return true
	})
}
