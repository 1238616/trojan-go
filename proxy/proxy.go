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
)

const Name = "PROXY"

const (
// relayBufferSize used by the legacy code path has moved to buffer.go,
// along with MaxPacketSize for the UDP relay.
)

// Proxy relay connections and packets
type Proxy struct {
	sources        []tunnel.Server
	sink           tunnel.Client
	ctx            context.Context
	cancel         context.CancelFunc
	connSeq        atomic.Int64
	profiler       *Profiler
	clusterRouter  *cluster.ClusterRouter
	enableZeroCopy bool // splice(2) fast path for TCP→TCP relay
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
							// Only register as slow target on timeout (not on
							// connection refused, DNS failure, etc.)
							if p.clusterRouter != nil && dialRTT >= 5*time.Second {
								addr := inbound.Metadata().Address
								host := addr.DomainName
								if host == "" && addr.IP != nil {
									host = addr.IP.String()
								}
								if host != "" && !isMuxMagicDomain(host) {
									p.clusterRouter.RegisterSlowTarget(host, addr.Port, dialRTT)
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
						if !fellBackToPeer && p.clusterRouter != nil {
							addr := inbound.Metadata().Address
							host := addr.DomainName
							if host == "" && addr.IP != nil {
								host = addr.IP.String()
							}
							if host != "" && !isMuxMagicDomain(host) {
								p.clusterRouter.RegisterSlowTarget(host, addr.Port, dialRTT)
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

					var ttfbDone atomic.Bool

					// Splice fast path: when enable_zero_copy is set and
					// both sides expose a raw TCP FD, use splice(2) to
					// relay data through a kernel pipe — zero user-space
					// copies per byte. Byte counting is maintained via
					// the SpliceRelayCounted callback.
					if p.enableZeroCopy {
						srcTCP := extractTCPConn(outbound)
						dstTCP := extractTCPConn(inbound)
						if srcTCP != nil && dstTCP != nil {
							errChan := make(chan error, 2)
							// Upload: inbound → outbound via splice
							go func() {
								_, err := common.SpliceRelayCounted(dstTCP, srcTCP, func(n int64) {
									entry.AddUpload(n)
								})
								errChan <- err
							}()
							// Download: outbound → inbound via splice
							go func() {
								_, err := common.SpliceRelayCounted(srcTCP, dstTCP, func(n int64) {
									entry.AddDownload(n)
									if ttfbDone.CompareAndSwap(false, true) && metrics != nil {
										metrics.RecordTTFB(time.Since(dialDoneAt))
									}
								})
								errChan <- err
							}()
							select {
							case err = <-errChan:
								if err != nil {
									if strings.Contains(err.Error(), "closed pipe") {
										log.DebugKV("conn relay splice teardown",
											"conn_id", string(connID),
											"target", target,
											"err", err)
									} else {
										log.ErrorKV("conn relay splice error", "conn_id", string(connID), "target", target, "err", err)
									}
								}
								closeReason = classifyCloseReason(err)
							case <-p.ctx.Done():
								log.DebugKV("shutting down conn relay (splice)", "conn_id", string(connID))
								return
							}
							select {
							case <-errChan:
							case <-p.ctx.Done():
							}
							log.DebugKV("conn relay ends (splice)", "conn_id", string(connID), "target", target, "exit", exitNode, "reason", closeReason)
							return
						}
					}

					errChan := make(chan error, 2)
					// inbound -> outbound (upload)
					go func() {
						buf := getBuf()
						defer putBuf(buf)
						_, err := copyBuffer(outbound, &countingReader{reader: inbound, entry: entry, upload: true}, *buf)
						errChan <- err
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
						errChan <- err
					}()
					select {
					case err = <-errChan:
						if err != nil {
							if strings.Contains(err.Error(), "closed pipe") {
								log.DebugKV("conn relay mux teardown",
									"conn_id", string(connID),
									"target", target,
									"err", err)
							} else {
								log.ErrorKV("conn relay error", "conn_id", string(connID), "target", target, "err", err)
							}
						}
						closeReason = classifyCloseReason(err)
					case <-p.ctx.Done():
						log.DebugKV("shutting down conn relay", "conn_id", string(connID))
						return
					}
					// Drain the second direction so neither goroutine lingers.
					select {
					case <-errChan:
					case <-p.ctx.Done():
					}
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

	// Splice zero-copy: propagate config to the proxy relay loop.
	if cfg.EnableZeroCopy {
		p.enableZeroCopy = true
		log.Info("proxy: enable_zero_copy=true (splice fast path active)")
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
		router, crErr := cluster.NewClusterRouter(p.ctx, &clusterCfg.Cluster)
		if crErr != nil {
			log.Warn("cluster router init failed: ", crErr)
		} else if router != nil {
			p.clusterRouter = router
			log.Infof("proxy: cluster router enabled, %d peers", len(clusterCfg.Cluster.Peers))
		}
	}

	return p, nil
}

// extractTCPConn tries to extract a *net.TCPConn from a tunnel.Conn.
// It checks for *freedom.Conn (via the unwrapper interface) and direct
// *net.TCPConn. Returns nil when the connection is TLS, mux, or other
// non-raw-TCP type.
func extractTCPConn(c tunnel.Conn) *net.TCPConn {
	type tcpUnwrapper interface {
		UnwrapTCPConn() *net.TCPConn
	}
	if u, ok := c.(tcpUnwrapper); ok {
		return u.UnwrapTCPConn()
	}
	return nil
}

func isMuxMagicDomain(host string) bool {
	switch host {
	case "MUX_CONN", "v1.mux.cool", "sp.mux.sing-box.arpa":
		return true
	}
	return false
}
