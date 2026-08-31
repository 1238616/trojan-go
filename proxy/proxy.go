package proxy

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/p4gefau1t/trojan-go/api/httpapi"
	"github.com/p4gefau1t/trojan-go/cluster"
	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel"
	"github.com/p4gefau1t/trojan-go/tunnel/freedom"
)

const Name = "PROXY"

const (
// relayBufferSize used by the legacy code path has moved to buffer.go,
// along with MaxPacketSize for the UDP relay.
)

// Proxy relay connections and packets
type Proxy struct {
	sources       []tunnel.Server
	sink          tunnel.Client
	ctx           context.Context
	cancel        context.CancelFunc
	connSeq       atomic.Int64
	profiler      *Profiler
	clusterRouter *cluster.ClusterRouter
	// fallbackSlowDial is the "this dial ran to its timeout" heuristic
	// used when the dial error lost its type information. Wired to the
	// freedom layer's configured dial timeout (see NewProxyFromConfigData).
	fallbackSlowDial time.Duration
}

func (p *Proxy) Run() error {
	// Start connection monitor HTTP API in background
	go func() {
		if err := httpapi.RunHTTPAPI(p.ctx); err != nil {
			log.Warn("conn monitor API error: ", err)
		}
	}()
	// Start debug profiler if enabled
	if p.profiler != nil {
		p.profiler.Start()
	}
	// Start cluster router if enabled
	if p.clusterRouter != nil {
		p.clusterRouter.Start()
	}
	p.relayConnLoop()
	p.relayPacketLoop()
	<-p.ctx.Done()
	return nil
}

func (p *Proxy) Close() error {
	p.cancel()
	if p.profiler != nil {
		p.profiler.Stop()
	}
	if p.clusterRouter != nil {
		p.clusterRouter.Stop()
	}
	p.sink.Close()
	for _, source := range p.sources {
		source.Close()
	}
	return nil
}

func (p *Proxy) relayConnLoop() {
	for _, source := range p.sources {
		go func(source tunnel.Server) {
			for {
				inbound, err := source.AcceptConn(nil)
				if err != nil {
					select {
					case <-p.ctx.Done():
						log.Debug("exiting")
						return
					default:
					}
					log.Error(common.NewError("failed to accept connection").Base(err))
					continue
				}
				go func(inbound tunnel.Conn) {
					defer inbound.Close()
					target := inbound.Metadata().Address.String()

					// Cluster routing: try relay through optimal peer first
					var outbound tunnel.Conn
					exitNode := "local"
					if p.clusterRouter != nil {
						relayConn, node, _ := p.clusterRouter.DialConn(inbound.Metadata().Address)
						if relayConn != nil {
							outbound = &clusterConn{Conn: relayConn, addr: inbound.Metadata().Address}
							exitNode = node
						}
					}
					if outbound == nil {
						var err error
						dialStart := time.Now()
						outbound, err = p.sink.DialConn(inbound.Metadata().Address, nil)
						dialRTT := time.Since(dialStart)
						fellBackToPeer := false
						if err != nil {
							// Only register as blocked/slow target when the
							// failure looks like a timeout (SYN dropped, i.e.
							// likely blocked) — not on connection refused or
							// DNS failure, where the target is simply down
							// and relaying through a peer can't help.
							// dialFailed=true marks local unreachable in the
							// route table so the next connection can relay
							// through a peer without waiting for a probe cycle.
							if p.clusterRouter != nil && isTimeoutLikeDial(err, dialRTT, p.fallbackSlowDial) {
								addr := inbound.Metadata().Address
								host := addr.DomainName
								if host == "" && addr.IP != nil {
									host = addr.IP.String()
								}
								if host != "" && !isMuxMagicDomain(host) {
									p.clusterRouter.RegisterSlowTarget(host, addr.Port, dialRTT, true)
								}
							}
							// Emergency fallback: route the in-flight connection
							// through any alive peer so it isn't dropped while
							// the urgent probe rebuilds the route table.
							if p.clusterRouter != nil {
								if relayConn, peerName, perr := p.clusterRouter.DialAnyPeer(inbound.Metadata().Address); perr == nil && relayConn != nil {
									log.InfoKV("proxy: emergency peer fallback after local dial failure",
										"target", target,
										"peer", peerName,
										"local_dial_err", err.Error(),
										"local_dial_ms", dialRTT.Milliseconds())
									outbound = &clusterConn{Conn: relayConn, addr: inbound.Metadata().Address}
									exitNode = peerName
									err = nil
									fellBackToPeer = true
								}
							}
							if err != nil {
								log.ErrorKV("proxy failed to dial connection", "target", target, "err", err)
								return
							}
						}
						// Successful local dial — register latency for route optimization.
						// Skip when we fell back to a peer; dialRTT then reflects the
						// failed local attempt, not a real success.
						// dialFailed=false: a slow-but-successful dial never marks
						// local unreachable — slow != blocked.
						if !fellBackToPeer && p.clusterRouter != nil {
							addr := inbound.Metadata().Address
							host := addr.DomainName
							if host == "" && addr.IP != nil {
								host = addr.IP.String()
							}
							if host != "" && !isMuxMagicDomain(host) {
								p.clusterRouter.RegisterSlowTarget(host, addr.Port, dialRTT, false)
							}
						}
					}
					defer outbound.Close()
					// dialDoneAt anchors the TTFB measurement: we record the
					// elapsed time from dial-success to the first non-zero
					// downstream Read so the dashboard reflects user-perceived
					// "open speed" of the origin.
					dialDoneAt := time.Now()

					// Register with connection monitor and keep the *connEntry
					// pointer so the data path stays lock-free.
					monitor := connmonitor.Global()
					metrics := connmonitor.GlobalMetrics()
					connID := strconv.AppendInt([]byte("conn-"), p.connSeq.Add(1), 10)
					entry := monitor.RegisterEntry(string(connID), target)
					metrics.RecordConnOpen()
					var closeReason connmonitor.CloseReason = connmonitor.CloseReasonEOF
					defer func() {
						metrics.RecordConnClose(closeReason)
						monitor.UnregisterEntry(entry)
					}()

					closeReason = relayBidirectional(p.ctx, inbound, outbound, entry, metrics, dialDoneAt, string(connID), target)
					log.DebugKV("conn relay ends", "conn_id", string(connID), "target", target, "exit", exitNode, "reason", closeReason)
				}(inbound)
			}
		}(source)
	}
}

func (p *Proxy) relayPacketLoop() {
	metrics := connmonitor.GlobalMetrics()
	for _, source := range p.sources {
		go func(source tunnel.Server) {
			for {
				inbound, err := source.AcceptPacket(nil)
				if err != nil {
					select {
					case <-p.ctx.Done():
						log.Debug("exiting")
						return
					default:
					}
					log.Error(common.NewError("failed to accept packet").Base(err))
					continue
				}
				go func(inbound tunnel.PacketConn) {
					defer inbound.Close()
					outbound, err := p.sink.DialPacket(nil)
					if err != nil {
						log.Error(common.NewError("proxy failed to dial packet").Base(err))
						return
					}
					defer outbound.Close()

					// Phase 1: packet-flow lifecycle metrics. The counter
					// bookkeeping is done once per flow; per-packet
					// accounting lives inside each goroutine.
					metrics.RecordPacketOpen()
					var closeReason connmonitor.CloseReason = connmonitor.CloseReasonEOF
					defer func() {
						metrics.RecordPacketClose(closeReason)
					}()

					errChan := make(chan error, 2)
					copyPacket := func(a, b tunnel.PacketConn) {
						// Phase 1: borrow an 8 KiB buffer from the packet
						// pool instead of per-flow make([]byte, 8192).
						// ReadWithMetadata is synchronous so there is no
						// aliasing issue.
						bp := getPacketBuf()
						defer putPacketBuf(bp)
						buf := *bp
						for {
							n, metadata, err := a.ReadWithMetadata(buf)
							if err != nil {
								errChan <- err
								return
							}
							if n == 0 {
								errChan <- nil
								return
							}
							_, err = b.WriteWithMetadata(buf[:n], metadata)
							if err != nil {
								errChan <- err
								return
							}
						}
					}
					go copyPacket(inbound, outbound)
					go copyPacket(outbound, inbound)
					select {
					case err = <-errChan:
						if err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
							log.Error(err)
						}
						closeReason = classifyCloseReason(err)
					case <-p.ctx.Done():
						log.Debug("shutting down packet relay")
					}
					log.Debug("packet relay ends")
				}(inbound)
			}
		}(source)
	}
}

func NewProxy(ctx context.Context, cancel context.CancelFunc, sources []tunnel.Server, sink tunnel.Client) *Proxy {
	return &Proxy{
		sources: sources,
		sink:    sink,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// NewProxyWithProfiler creates a Proxy with an optional debug profiler attached.
func NewProxyWithProfiler(ctx context.Context, cancel context.CancelFunc, sources []tunnel.Server, sink tunnel.Client, profiler *Profiler) *Proxy {
	return &Proxy{
		sources:  sources,
		sink:     sink,
		ctx:      ctx,
		cancel:   cancel,
		profiler: profiler,
	}
}

// countingReader wraps an io.Reader and reports bytes via the cached
// *connEntry pointer obtained at registration time. This avoids the map
// lookup and read lock that the legacy monitor.Record* path would take on
// every Read.
//
// On the download direction, the first non-zero Read also records TTFB
// (time-from-dial-success-to-first-byte). The flag is a single atomic.Bool
// so the hot path performs at most one CAS per connection.
type countingReader struct {
	reader    io.Reader
	entry     *connmonitor.Entry
	upload    bool
	ttfbStart time.Time
	ttfbDone  *atomic.Bool
	metrics   *connmonitor.Metrics
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if n > 0 {
		if c.upload {
			c.entry.AddUpload(int64(n))
		} else {
			c.entry.AddDownload(int64(n))
			if c.ttfbDone != nil && c.metrics != nil && c.ttfbDone.CompareAndSwap(false, true) {
				c.metrics.RecordTTFB(time.Since(c.ttfbStart))
			}
		}
	}
	return n, err
}

// relayResult tags which direction of the bidirectional relay finished
// so the teardown logic can propagate the ending correctly.
type relayResult struct {
	upload bool
	err    error
}

// relayBidirectional copies data between inbound and outbound in both
// directions until the relay ends, and returns the reason it ended.
//
// Termination semantics (issue #1):
//
//   - When the first direction ends with a real error, both ends are
//     closed immediately so the second goroutine unblocks at once
//     instead of hanging on Read until TCP keepalive fires (~60s with
//     the freedom defaults).
//   - When the first direction ends with a clean EOF, the half-close is
//     propagated to the opposite connection when it supports CloseWrite
//     (raw TCP, TLS), letting the remaining direction finish naturally —
//     e.g. the origin completes and closes its response only after it
//     has seen the request-side EOF.
//   - A clean EOF that cannot be signalled to the peer (no CloseWrite
//     support, e.g. mux/tunnel streams) closes both ends as well: the
//     peer would otherwise wait for data that is never coming.
func relayBidirectional(ctx context.Context, inbound, outbound net.Conn, entry *connmonitor.Entry, metrics *connmonitor.Metrics, dialDoneAt time.Time, connID, target string) connmonitor.CloseReason {
	var ttfbDone atomic.Bool
	errChan := make(chan relayResult, 2)
	// inbound -> outbound (upload)
	go func() {
		buf := getBuf()
		defer putBuf(buf)
		_, err := copyBuffer(outbound, &countingReader{reader: inbound, entry: entry, upload: true}, *buf)
		errChan <- relayResult{upload: true, err: err}
	}()
	// outbound -> inbound (download); also flags first byte for TTFB.
	go func() {
		buf := getBuf()
		defer putBuf(buf)
		_, err := copyBuffer(inbound, &countingReader{
			reader:    outbound,
			entry:     entry,
			upload:    false,
			ttfbStart: dialDoneAt,
			ttfbDone:  &ttfbDone,
			metrics:   metrics,
		}, *buf)
		errChan <- relayResult{upload: false, err: err}
	}()

	var first relayResult
	select {
	case first = <-errChan:
	case <-ctx.Done():
		log.DebugKV("shutting down conn relay", "conn_id", connID)
		return connmonitor.CloseReasonOther
	}
	if first.err != nil {
		if strings.Contains(first.err.Error(), "closed pipe") {
			log.DebugKV("conn relay mux teardown",
				"conn_id", connID,
				"target", target,
				"err", first.err)
		} else {
			log.ErrorKV("conn relay error", "conn_id", connID, "target", target, "err", first.err)
		}
	}

	// Decide whether the second direction may finish on its own (clean
	// EOF + half-close propagated) or whether both ends must be closed
	// now to guarantee the remaining goroutine unblocks.
	var propagated bool
	if first.err == nil {
		if first.upload {
			propagated = halfClose(outbound)
		} else {
			propagated = halfClose(inbound)
		}
	}
	if !propagated {
		inbound.Close()
		outbound.Close()
	}

	// Wait for the second direction. With the ends closed (or the
	// half-close signalled to the peer) it cannot outlive the relay by
	// more than the peer's reaction time.
	select {
	case <-errChan:
	case <-ctx.Done():
	}
	return classifyCloseReason(first.err)
}

// halfClose propagates a write-side EOF to c when the connection
// supports it (*net.TCPConn, *tls.Conn, freedom.Conn, …). It returns
// true when the peer will actually observe the EOF, i.e. the relay can
// leave the opposite direction open and let it end naturally.
func halfClose(c net.Conn) bool {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := c.(closeWriter); ok {
		return cw.CloseWrite() == nil
	}
	return false
}

// classifyCloseReason maps a relay error to the metrics close-reason enum so
// the dashboard can attribute disconnects (timeouts, peer resets, auth, …)
// rather than only counting flat "closed" events.
func classifyCloseReason(err error) connmonitor.CloseReason {
	if err == nil || errors.Is(err, io.EOF) {
		return connmonitor.CloseReasonEOF
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return connmonitor.CloseReasonTimeout
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "use of closed network connection"):
		return connmonitor.CloseReasonReset
	case strings.Contains(msg, "auth"),
		strings.Contains(msg, "password"),
		strings.Contains(msg, "hash"):
		return connmonitor.CloseReasonAuthFail
	}
	return connmonitor.CloseReasonOther
}

// isTimeoutLikeDial reports whether a failed local dial looks like a
// reachability failure (SYN dropped → timeout, the signature of a blocked
// target) rather than a fast reject (connection refused, DNS failure) where
// the target is simply down and relaying through a peer can't help.
//
// It prefers the error's own timeout signal (which respects whatever dial
// timeout the transport layer is configured with) and only falls back to
// comparing dialRTT against the configured slow-dial threshold when the
// error type was lost by wrapping.
func isTimeoutLikeDial(err error, dialRTT, slowDial time.Duration) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if os.IsTimeout(err) {
		return true
	}
	// Last-resort heuristic for wrapped errors that lost their type: a dial
	// that consumed the full configured timeout window was almost certainly
	// blocked rather than refused.
	return slowDial > 0 && dialRTT >= slowDial
}

// clusterConn adapts a net.Conn (from cluster relay) to tunnel.Conn.
type clusterConn struct {
	net.Conn
	addr *tunnel.Address
}

func (c *clusterConn) Metadata() *tunnel.Metadata {
	return &tunnel.Metadata{Address: c.addr}
}

type Creator func(ctx context.Context) (*Proxy, error)

var creators = make(map[string]Creator)

func RegisterProxyCreator(name string, creator Creator) {
	creators[name] = creator
}

func NewProxyFromConfigData(data []byte, isJSON bool) (*Proxy, error) {
	// create a unique context for each proxy instance to avoid duplicated authenticator
	ctx := context.WithValue(context.Background(), Name+"_ID", rand.Int())
	var err error
	if isJSON {
		ctx, err = config.WithJSONConfig(ctx, data)
		if err != nil {
			return nil, err
		}
	} else {
		ctx, err = config.WithYAMLConfig(ctx, data)
		if err != nil {
			return nil, err
		}
	}
	cfg := config.FromContext(ctx, Name).(*Config)

	// Phase 1: wire tuning knobs before the first relay goroutine is
	// spawned so that the hot path sees the final values. Values are
	// logged for operator visibility.
	if cfg.RelayBufferSize != 0 {
		eff := SetRelayBufferSize(cfg.RelayBufferSize)
		log.Infof("proxy: relay_buffer_size=%d (requested %d)", eff, cfg.RelayBufferSize)
	}
	if cfg.BackpressureThresh > 0 {
		connmonitor.GlobalMetrics().SetBackpressureThresh(cfg.BackpressureThresh)
		log.Infof("proxy: backpressure_thresh=%.3f", cfg.BackpressureThresh)
	}

	// Phase 4: GC tuning
	if cfg.GOGC > 0 {
		debug.SetGCPercent(cfg.GOGC)
		log.Infof("proxy: gogc=%d", cfg.GOGC)
	}
	if cfg.MemLimitMB > 0 {
		debug.SetMemoryLimit(cfg.MemLimitMB * 1024 * 1024)
		log.Infof("proxy: mem_limit_mb=%d", cfg.MemLimitMB)
	}

	create, ok := creators[strings.ToUpper(cfg.RunType)]
	if !ok {
		return nil, common.NewError("unknown proxy type: " + cfg.RunType)
	}
	log.SetLogLevel(log.LogLevel(cfg.LogLevel))
	if cfg.LogFile != "" {
		file, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, common.NewError("failed to open log file").Base(err)
		}
		log.SetOutput(file)
	}
	p, err := create(ctx)
	if err != nil {
		return nil, err
	}

	// Debug profiler: attach if config.debug is true.
	log.Infof("proxy: debug=%v", cfg.Debug)
	if cfg.Debug {
		profiler, profErr := NewProfiler(p.ctx)
		if profErr != nil {
			log.Warn("profiler init failed, continuing without debug: ", profErr)
		} else {
			p.profiler = profiler
			log.Info("proxy: debug profiler enabled, output dir: profile_debug/")
		}
	}

	// Cluster router: attach if cluster.enabled is true.
	clusterCfg := config.FromContext(ctx, cluster.Name).(*cluster.TopLevelConfig)
	if clusterCfg.Cluster.Enabled {
		// Tie the cluster fallback "slow dial" heuristic to the freedom
		// layer's configured dial timeout instead of a magic number, so a
		// real timeout is still recognized when the operator configures a
		// dial_timeout shorter than the old hardcoded 5s.
		p.fallbackSlowDial = 5 * time.Second
		if freedomCfg, ok := config.FromContext(ctx, freedom.Name).(*freedom.Config); ok && freedomCfg != nil && freedomCfg.TCP.DialTimeout > 0 {
			p.fallbackSlowDial = time.Duration(freedomCfg.TCP.DialTimeout) * time.Second
		}

		router, crErr := cluster.NewClusterRouter(p.ctx, &clusterCfg.Cluster)
		if crErr != nil {
			log.Warn("cluster router init failed: ", crErr)
		} else if router != nil {
			p.clusterRouter = router
			log.Infof("proxy: cluster router enabled, %d peers (fallback slow-dial=%s)",
				len(clusterCfg.Cluster.Peers), p.fallbackSlowDial)
		}
	}

	return p, nil
}

func isMuxMagicDomain(host string) bool {
	switch host {
	case "MUX_CONN", "v1.mux.cool", "sp.mux.sing-box.arpa":
		return true
	}
	return false
}
