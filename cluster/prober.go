package cluster

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/p4gefau1t/trojan-go/log"
)

// ProbeTarget is a destination to measure latency against.
type ProbeTarget struct {
	Host string
	Port int
}

func (pt ProbeTarget) Key() string {
	return net.JoinHostPort(pt.Host, strconv.Itoa(pt.Port))
}

// DynamicTarget is registered by actual traffic when dial RTT exceeds threshold.
type DynamicTarget struct {
	Host        string
	Port        int
	FirstSeenAt time.Time
	LastDialRTT time.Duration
}

const maxDynamicTargets = 50

// Prober periodically measures latency from each peer (and local) to
// registered targets. It is fully asynchronous and never blocks data paths.
type Prober struct {
	ctx              context.Context
	cancel           context.CancelFunc
	peers            []PeerConfig
	peerDialers      map[string]*PeerDialer
	staticTargets    []ProbeTarget
	dynamicTargets   sync.Map // map[string]*DynamicTarget
	routeTable       *RouteTable
	interval         time.Duration
	timeout          time.Duration
	latencyThreshold time.Duration
	localName        string
	metrics          *ClusterMetrics
	peerAlive        sync.Map // map[string]bool — peer connectivity status
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

	// Parse static targets from config
	var staticTargets []ProbeTarget
	for _, t := range cfg.Targets {
		// Static targets use port 443 by default for probing
		host, portStr, err := net.SplitHostPort(t)
		if err != nil {
			// No port specified, try parsing as CIDR or bare IP
			// For CIDR targets, we can't probe directly - skip
			// For domain/IP targets, use port 443
			if ip := net.ParseIP(t); ip != nil {
				staticTargets = append(staticTargets, ProbeTarget{Host: t, Port: 443})
			}
			continue
		}
		port, _ := strconv.Atoi(portStr)
		if port > 0 {
			staticTargets = append(staticTargets, ProbeTarget{Host: host, Port: port})
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
// exceeds latencyThreshold. It is lock-free (sync.Map) and safe to call
// from hot paths. When the dial actually timed out (RTT >= dialTimeout),
// the route table is immediately updated to mark local as unreachable
// so subsequent connections can relay through a peer without waiting
// for the next probe cycle.
func (p *Prober) RegisterSlowTarget(host string, port int, dialRTT time.Duration) {
	if dialRTT < p.latencyThreshold {
		log.DebugKV("cluster: dial below threshold, skip",
			"host", host, "port", port,
			"dial_rtt_ms", dialRTT.Milliseconds(),
			"threshold_ms", p.latencyThreshold.Milliseconds())
		return
	}
	key := net.JoinHostPort(host, strconv.Itoa(port))
	isNew := false
	if _, loaded := p.dynamicTargets.LoadOrStore(key, &DynamicTarget{
		Host:        host,
		Port:        port,
		FirstSeenAt: time.Now(),
		LastDialRTT: dialRTT,
	}); loaded {
		// Existing target — update RTT
		if v, ok := p.dynamicTargets.Load(key); ok {
			v.(*DynamicTarget).LastDialRTT = dialRTT
			v.(*DynamicTarget).FirstSeenAt = time.Now()
		}
	} else {
		// New target — enforce cap
		if p.DynamicTargetCount() > maxDynamicTargets {
			p.dynamicTargets.Delete(key)
			log.DebugKV("cluster: dynamic target cap reached, dropping",
				"host", host, "port", port,
				"max", maxDynamicTargets)
			return
		}
		isNew = true
	}

	// When dial timed out (>= 5s), immediately mark local as unreachable
	// in the route table so the next connection can relay through a peer.
	if dialRTT >= 5*time.Second {
		p.routeTable.Update(key, p.localName, -1)
		log.InfoKV("cluster: dial timeout, local marked unreachable",
			"host", host, "port", port,
			"dial_rtt_ms", dialRTT.Milliseconds())
		if isNew {
			go p.probeTargetAllPeers(ProbeTarget{Host: host, Port: port})
		}
		return
	}

	log.InfoKV("cluster: slow target registered",
		"host", host, "port", port,
		"dial_rtt_ms", dialRTT.Milliseconds(),
		"threshold_ms", p.latencyThreshold.Milliseconds())
}

// DynamicTargetCount returns the number of dynamically registered targets.
func (p *Prober) DynamicTargetCount() int {
	count := 0
	p.dynamicTargets.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
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
	log.Infof("cluster: probing %d targets across %d peers", len(targets), len(p.peers))

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
			log.InfoKV("cluster: probe local",
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
				rtt := dialer.Probe(target)
				p.routeTable.Update(target.Key(), peer.Name, rtt)
				log.InfoKV("cluster: probe peer",
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
func (p *Prober) probePeerAlive() {
	var wg sync.WaitGroup
	for _, peer := range p.peers {
		peer := peer
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialer, ok := p.peerDialers[peer.Name]
			if !ok {
				p.peerAlive.Store(peer.Name, false)
				return
			}
			rtt := dialer.CheckAlive(p.timeout)
			alive := rtt >= 0
			p.peerAlive.Store(peer.Name, alive)
			if alive {
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
	v, ok := p.peerAlive.Load(peerName)
	if !ok {
		return false
	}
	return v.(bool)
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
			rtt := dialer.Probe(target)
			p.routeTable.Update(target.Key(), peer.Name, rtt)
			log.InfoKV("cluster: urgent probe peer",
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
		if dt.FirstSeenAt.Before(cutoff) {
			p.dynamicTargets.Delete(key)
			log.DebugKV("cluster: evicted stale probe target", "host", dt.Host)
		}
		return true
	})
}
