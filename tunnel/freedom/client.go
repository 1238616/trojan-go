package freedom

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/txthinking/socks5"
	"golang.org/x/net/proxy"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// Phase 1: DNS resolver uses the pure-Go resolver so the dashboard sees
// real DNS latency instead of CGo / NSS black-box time. Operators can
// disable this by setting freedom_dns_use_cgo = true in their config.
var defaultResolver = &net.Resolver{PreferGo: true}

// dialSplit performs DNS resolution followed by a TCP connect to one of
// the resolved IPs, reporting each phase separately to the metrics
// collector. The original combined OriginDial counter is still bumped
// for dashboard backward compatibility.
//
// resolvedIP, when non-nil, is an address an upstream layer (e.g. the
// cluster router) already resolved for this same connection; the DNS phase
// is skipped entirely so a single connection triggers at most one resolver
// round-trip across all layers (issue #4).
func dialSplit(ctx context.Context, network, addr string, resolvedIP net.IP, d *net.Dialer, metrics *connmonitor.Metrics) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Phase 2: per-target accounting. host is used as the bucket key;
	// when target cap is reached, GetTarget folds further hosts into
	// the synthetic "__other__" bucket automatically.
	tgt := metrics.GetTarget(host)

	// Fast path: addr is already an IP, skip the DNS phase.
	if ip := net.ParseIP(host); ip != nil {
		tcpStart := time.Now()
		conn, derr := d.DialContext(ctx, network, addr)
		tcpDur := time.Since(tcpStart)
		metrics.RecordTCPDial(tcpDur)
		if tgt != nil {
			tgt.RecordDial(tcpDur, derr == nil)
		}
		return conn, derr
	}

	var ips []net.IPAddr
	var dnsDur time.Duration
	if resolvedIP != nil {
		// Reuse the upstream resolution instead of querying again.
		ips = []net.IPAddr{{IP: resolvedIP}}
	} else {
		// Phase 1: DNS resolve
		dnsStart := time.Now()
		var rerr error
		ips, rerr = defaultResolver.LookupIPAddr(ctx, host)
		dnsDur = time.Since(dnsStart)
		metrics.RecordDNSResolve(dnsDur, rerr == nil && len(ips) > 0, host)
		if rerr != nil {
			if tgt != nil {
				tgt.RecordDial(dnsDur, false)
			}
			return nil, rerr
		}
		if len(ips) == 0 {
			dnsErr := &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
			if tgt != nil {
				tgt.RecordDial(dnsDur, false)
			}
			return nil, dnsErr
		}
	}

	// Filter by network: "tcp4" -> IPv4 only, "tcp6" -> IPv6 only.
	wantV4 := network == "tcp4" || network == "udp4"
	wantV6 := network == "tcp6" || network == "udp6"
	filtered := ips[:0]
	for _, ip := range ips {
		if wantV4 && ip.IP.To4() == nil {
			continue
		}
		if wantV6 && ip.IP.To4() != nil {
			continue
		}
		filtered = append(filtered, ip)
	}
	if len(filtered) == 0 {
		// fall back to original list so the error comes from DialContext
		// rather than "no such host".
		filtered = ips
	}

	// Phase 1: TCP connect to first resolved IP. When the first pick
	// fails we fall through to subsequent IPs so that a flaky IPv6
	// address doesn't break a flow that IPv4 could have served.
	var lastErr error
	var totalDur time.Duration
	tcpStartAll := time.Now()
	for _, ip := range filtered {
		target := net.JoinHostPort(ip.IP.String(), port)
		tcpStart := time.Now()
		conn, derr := d.DialContext(ctx, network, target)
		tcpDur := time.Since(tcpStart)
		metrics.RecordTCPDial(tcpDur)
		if derr == nil {
			totalDur = time.Since(tcpStartAll) + dnsDur
			if tgt != nil {
				tgt.RecordDial(totalDur, true)
			}
			return conn, nil
		}
		lastErr = derr
	}
	totalDur = time.Since(tcpStartAll) + dnsDur
	if tgt != nil {
		tgt.RecordDial(totalDur, false)
	}
	return nil, lastErr
}

// classifyDialError maps a net.Dialer error to the OriginDialKind enum so the
// dashboard can break down outbound failures by root cause (DNS / refused /
// timeout / unreachable). It is a pure observability helper and does not
// change any control-flow decisions.
func classifyDialError(err error) connmonitor.OriginDialKind {
	if err == nil {
		return connmonitor.OriginDialOK
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return connmonitor.OriginDialDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return connmonitor.OriginDialRefused
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return connmonitor.OriginDialUnreachable
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return connmonitor.OriginDialTimeout
	}
	if os.IsTimeout(err) {
		return connmonitor.OriginDialTimeout
	}
	return connmonitor.OriginDialOther
}

type Client struct {
	preferIPv4   bool
	noDelay      bool
	keepAlive    bool
	readBuffer   int // Phase 4: SO_RCVBUF (0 = OS default)
	writeBuffer  int // Phase 4: SO_SNDBUF (0 = OS default)
	keepIdleSec  int // Phase 4: TCP_KEEPIDLE (0 = kernel default)
	keepIntvlSec int // Phase 4: TCP_KEEPINTVL (0 = kernel default)
	keepCnt      int // Phase 4: TCP_KEEPCNT (0 = kernel default)
	dialTimeout  int // Phase 4: dial timeout in seconds
	ctx          context.Context
	cancel       context.CancelFunc
	forwardProxy bool
	proxyAddr    *tunnel.Address
	username     string
	password     string
}

func (c *Client) DialConn(addr *tunnel.Address, _ tunnel.Tunnel) (tunnel.Conn, error) {
	// forward proxy
	if c.forwardProxy {
		var auth *proxy.Auth
		if c.username != "" {
			auth = &proxy.Auth{
				User:     c.username,
				Password: c.password,
			}
		}
		dialer, err := proxy.SOCKS5("tcp", c.proxyAddr.String(), auth, proxy.Direct)
		if err != nil {
			return nil, common.NewError("freedom failed to init socks dialer")
		}
		conn, err := dialer.Dial("tcp", addr.String())
		if err != nil {
			return nil, common.NewError("freedom failed to dial target address via socks proxy " + addr.String()).Base(err)
		}
		return &Conn{
			Conn: conn,
		}, nil
	}
	network := "tcp"
	if c.preferIPv4 {
		network = "tcp4"
	}
	// Phase 4: use a Dialer with timeout and keepalive.
	dialer := &net.Dialer{
		Timeout:   time.Duration(c.dialTimeout) * time.Second,
		KeepAlive: time.Duration(c.keepIdleSec) * time.Second,
		Control:   c.controlHook,
	}
	dialStart := time.Now()
	metrics := connmonitor.GlobalMetrics()
	// When an upstream layer already resolved this domain for the current
	// connection (cluster router decision path), hand the result over so
	// the dial doesn't spend a second resolver round-trip on it (issue #4).
	var resolvedIP net.IP
	if addr.AddressType == tunnel.DomainName && addr.IP != nil {
		resolvedIP = addr.IP
	}
	tcpConn, err := dialSplit(c.ctx, network, addr.String(), resolvedIP, dialer, metrics)
	dialDur := time.Since(dialStart)
	connmonitor.GlobalMetrics().RecordOriginDial(dialDur, classifyDialError(err))
	if err != nil {
		log.ErrorKV("freedom dial failed", "target", addr.String(), "dur_ms", dialDur.Milliseconds(), "err", err)
		return nil, common.NewError("freedom failed to dial " + addr.String()).Base(err)
	}
	log.DebugKV("freedom dial ok", "target", addr.String(), "dur_ms", dialDur.Milliseconds())

	tcpConn.(*net.TCPConn).SetKeepAlive(c.keepAlive)
	tcpConn.(*net.TCPConn).SetNoDelay(c.noDelay)

	// Phase 4: explicit socket buffer sizes.
	if c.readBuffer > 0 {
		tcpConn.(*net.TCPConn).SetReadBuffer(c.readBuffer)
	}
	if c.writeBuffer > 0 {
		tcpConn.(*net.TCPConn).SetWriteBuffer(c.writeBuffer)
	}

	// Phase 4: aggressive keepalive via syscall. The socket options are
	// Linux-only; applyKeepalive is a no-op on other platforms
	// (keepalive_other.go), which also keeps this file free of
	// platform-specific syscall signatures (issue #14: Windows
	// cross-compilation used to break here).
	if c.keepAlive {
		if raw, err := tcpConn.(*net.TCPConn).SyscallConn(); err == nil {
			c.applyKeepalive(raw)
		}
	}
	return &Conn{
		Conn: tcpConn,
	}, nil
}

func (c *Client) DialPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	if c.forwardProxy {
		socksClient, err := socks5.NewClient(c.proxyAddr.String(), c.username, c.password, 0, 0)
		common.Must(err)
		if err := socksClient.Negotiate(&net.TCPAddr{}); err != nil {
			return nil, common.NewError("freedom failed to negotiate socks").Base(err)
		}
		a, addr, port, err := socks5.ParseAddress("1.1.1.1:53") // useless address
		common.Must(err)
		resp, err := socksClient.Request(socks5.NewRequest(socks5.CmdUDP, a, addr, port))
		if err != nil {
			return nil, common.NewError("freedom failed to dial udp to socks").Base(err)
		}
		// TODO fix hardcoded localhost
		packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return nil, common.NewError("freedom failed to listen udp").Base(err)
		}
		socksAddr, err := net.ResolveUDPAddr("udp", resp.Address())
		if err != nil {
			return nil, common.NewError("freedom recv invalid socks bind addr").Base(err)
		}
		return &SocksPacketConn{
			PacketConn:  packetConn,
			socksAddr:   socksAddr,
			socksClient: socksClient,
		}, nil
	}
	network := "udp"
	if c.preferIPv4 {
		network = "udp4"
	}
	udpConn, err := net.ListenPacket(network, "")
	if err != nil {
		return nil, common.NewError("freedom failed to listen udp socket").Base(err)
	}
	return &PacketConn{
		UDPConn: udpConn.(*net.UDPConn),
	}, nil
}

func (c *Client) Close() error {
	c.cancel()
	return nil
}

func NewClient(ctx context.Context, _ tunnel.Client) (*Client, error) {
	cfg := config.FromContext(ctx, Name).(*Config)
	addr := tunnel.NewAddressFromHostPort("tcp", cfg.ForwardProxy.ProxyHost, cfg.ForwardProxy.ProxyPort)
	ctx, cancel := context.WithCancel(ctx)
	return &Client{
		ctx:          ctx,
		cancel:       cancel,
		noDelay:      cfg.TCP.NoDelay,
		keepAlive:    cfg.TCP.KeepAlive,
		preferIPv4:   cfg.TCP.PreferIPV4,
		readBuffer:   cfg.TCP.ReadBuffer,
		writeBuffer:  cfg.TCP.WriteBuffer,
		keepIdleSec:  cfg.TCP.KeepIdleSec,
		keepIntvlSec: cfg.TCP.KeepIntvlSec,
		keepCnt:      cfg.TCP.KeepCnt,
		dialTimeout:  cfg.TCP.DialTimeout,
		forwardProxy: cfg.ForwardProxy.Enabled,
		proxyAddr:    addr,
		username:     cfg.ForwardProxy.Username,
		password:     cfg.ForwardProxy.Password,
	}, nil
}
