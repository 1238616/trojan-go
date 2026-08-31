package trojan

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p4gefau1t/trojan-go/api"
	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/redirector"
	"github.com/p4gefau1t/trojan-go/statistic"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/statistic/memory"
	"github.com/p4gefau1t/trojan-go/statistic/mysql"
	"github.com/p4gefau1t/trojan-go/tunnel"
	"github.com/p4gefau1t/trojan-go/tunnel/mux"
	"github.com/p4gefau1t/trojan-go/tunnel/muxcool"
	"github.com/p4gefau1t/trojan-go/tunnel/singmux"
)

// InboundConn is a trojan inbound connection
type InboundConn struct {
	sent uint64
	recv uint64

	net.Conn
	auth      statistic.Authenticator
	user      statistic.User
	hash      string
	metadata  *tunnel.Metadata
	ip        string
	closeOnce sync.Once

	// Phase 3: per-user metrics (nil when disabled)
	userMetrics *connmonitor.PerUserMetrics
}

func (c *InboundConn) Metadata() *tunnel.Metadata {
	return c.metadata
}

func (c *InboundConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	atomic.AddUint64(&c.sent, uint64(n))
	c.user.AddTraffic(n, 0)
	if c.userMetrics != nil {
		c.userMetrics.RecordUp(int64(n))
	}
	return n, err
}

func (c *InboundConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	atomic.AddUint64(&c.recv, uint64(n))
	c.user.AddTraffic(0, n)
	if c.userMetrics != nil {
		c.userMetrics.RecordDown(int64(n))
	}
	return n, err
}

func (c *InboundConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// Per-connection routine event: Debug, not Info (issue #3). The
		// traffic totals remain available through the connection monitor.
		log.Debug("user", c.hash, "from", c.Conn.RemoteAddr(), "tunneling to", c.metadata.Address, "closed",
			"sent:", common.HumanFriendlyTraffic(atomic.LoadUint64(&c.sent)), "recv:", common.HumanFriendlyTraffic(atomic.LoadUint64(&c.recv)))
		c.user.DelIP(c.ip)
		err = c.Conn.Close()
	})
	return err
}

// Auth performs trojan inbound authentication. It also records latency and
// failure-kind metrics so the dashboard can attribute auth failures to a
// stable cause (read_hash / invalid_hash / parse_host / ip_limit /
// read_crlf / read_metadata) rather than relying on substring matching of
// error messages elsewhere in the pipeline.
func (c *InboundConn) Auth() error {
	authStart := time.Now()
	failKind, err := c.authInternal()
	metrics := connmonitor.GlobalMetrics()
	metrics.RecordTrojanAuth(err == nil, time.Since(authStart), failKind)
	if err == nil {
		// Phase 3: wire per-user metrics after successful auth
		c.userMetrics = metrics.GetUser(c.hash)
		if c.userMetrics != nil {
			c.userMetrics.RecordConn()
		}
	} else if failKind != "" {
		// Record auth failure in per-user metrics if user was identified
		if len(c.hash) > 0 {
			if um := metrics.GetUser(c.hash); um != nil {
				um.RecordAuthFail()
			}
		}
	}
	return err
}

func (c *InboundConn) authInternal() (string, error) {
	userHash := [56]byte{}
	n, err := c.Conn.Read(userHash[:])
	if err != nil || n != 56 {
		return "read_hash", common.NewError("failed to read hash").Base(err)
	}

	valid, user := c.auth.AuthUser(string(userHash[:]))
	if !valid {
		return "invalid_hash", common.NewError("invalid hash:" + string(userHash[:]))
	}
	c.hash = string(userHash[:])
	c.user = user

	ip, _, err := net.SplitHostPort(c.Conn.RemoteAddr().String())
	if err != nil {
		return "parse_host", common.NewError("failed to parse host:" + c.Conn.RemoteAddr().String()).Base(err)
	}

	c.ip = ip
	ok := user.AddIP(ip)
	if !ok {
		return "ip_limit", common.NewError("ip limit reached")
	}

	crlf := [2]byte{}
	_, err = io.ReadFull(c.Conn, crlf[:])
	if err != nil {
		return "read_crlf", err
	}

	c.metadata = &tunnel.Metadata{}
	if err := c.metadata.ReadFrom(c.Conn); err != nil {
		return "read_metadata", err
	}

	_, err = io.ReadFull(c.Conn, crlf[:])
	if err != nil {
		return "read_crlf", err
	}
	return "", nil
}

// Server is a trojan tunnel server
type Server struct {
	auth        statistic.Authenticator
	redir       *redirector.Redirector
	redirAddr   *tunnel.Address
	underlay    tunnel.Server
	connChan    chan tunnel.Conn
	muxChan     chan tunnel.Conn
	singMuxChan chan tunnel.Conn
	muxCoolChan chan tunnel.Conn
	packetChan  chan tunnel.PacketConn
	ctx         context.Context
	cancel      context.CancelFunc
}

func (s *Server) Close() error {
	s.cancel()
	return s.underlay.Close()
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.underlay.AcceptConn(&Tunnel{})
		if err != nil { // Closing
			log.Error(common.NewError("trojan failed to accept conn").Base(err))
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			continue
		}
		go func(conn tunnel.Conn) {
			rewindConn := common.NewRewindConn(conn)
			rewindConn.SetBufferSize(128)
			defer rewindConn.StopBuffering()

			inboundConn := &InboundConn{
				Conn: rewindConn,
				auth: s.auth,
			}

			if err := inboundConn.Auth(); err != nil {
				rewindConn.Rewind()
				rewindConn.StopBuffering()
				log.Warn(common.NewError("connection with invalid trojan header from " + rewindConn.RemoteAddr().String()).Base(err))
				s.redir.Redirect(&redirector.Redirection{
					RedirectTo:  s.redirAddr,
					InboundConn: rewindConn,
				})
				return
			}

			rewindConn.StopBuffering()
			// Per-connection routine events log at Debug (issue #3);
			// authentication failures stay at Warn above.
			log.Debug("trojan conn from ", conn.RemoteAddr(),
				" cmd=", inboundConn.metadata.Command,
				" domain=", inboundConn.metadata.DomainName,
				" port=", inboundConn.metadata.Port,
				" atype=", inboundConn.metadata.AddressType)
			switch inboundConn.metadata.Command {
			case Connect:
				if inboundConn.metadata.DomainName == "MUX_CONN" {
					s.muxChan <- inboundConn
					log.Debug("trojan: routed to muxChan (trojan-go mux)")
				} else if inboundConn.metadata.DomainName == singmux.MagicDomain {
					s.singMuxChan <- inboundConn
					log.Debug("trojan: routed to singMuxChan (sing-mux)")
				} else if inboundConn.metadata.DomainName == muxcool.MagicDomain {
					s.muxCoolChan <- inboundConn
					log.Debug("trojan: routed to muxCoolChan (mux.cool)")
				} else {
					s.connChan <- inboundConn
					log.Debug("normal trojan connection")
				}

			case Associate:
				s.packetChan <- &PacketConn{
					Conn: inboundConn,
				}
				log.Debug("trojan udp connection")
			case Mux:
				s.muxChan <- inboundConn
				log.Debug("trojan: routed to muxChan (cmd=Mux 0x7f)")
			default:
				log.Error(common.NewError(fmt.Sprintf("unknown trojan command %d", inboundConn.metadata.Command)))
			}
		}(conn)
	}
}

func (s *Server) AcceptConn(nextTunnel tunnel.Tunnel) (tunnel.Conn, error) {
	switch nextTunnel.(type) {
	case *mux.Tunnel:
		select {
		case t := <-s.muxChan:
			return t, nil
		case <-s.ctx.Done():
			return nil, common.NewError("trojan client closed")
		}
	case *singmux.Tunnel:
		select {
		case t := <-s.singMuxChan:
			return t, nil
		case <-s.ctx.Done():
			return nil, common.NewError("trojan client closed")
		}
	case *muxcool.Tunnel:
		select {
		case t := <-s.muxCoolChan:
			return t, nil
		case <-s.ctx.Done():
			return nil, common.NewError("trojan client closed")
		}
	default:
		select {
		case t := <-s.connChan:
			return t, nil
		case <-s.ctx.Done():
			return nil, common.NewError("trojan client closed")
		}
	}
}

func (s *Server) AcceptPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	select {
	case t := <-s.packetChan:
		return t, nil
	case <-s.ctx.Done():
		return nil, common.NewError("trojan client closed")
	}
}

func NewServer(ctx context.Context, underlay tunnel.Server) (*Server, error) {
	cfg := config.FromContext(ctx, Name).(*Config)
	ctx, cancel := context.WithCancel(ctx)

	// TODO replace this dirty code
	var auth statistic.Authenticator
	var err error
	if cfg.MySQL.Enabled {
		log.Debug("mysql enabled")
		auth, err = statistic.NewAuthenticator(ctx, mysql.Name)
	} else {
		log.Debug("auth by config file")
		auth, err = statistic.NewAuthenticator(ctx, memory.Name)
	}
	if err != nil {
		cancel()
		return nil, common.NewError("trojan failed to create authenticator")
	}

	if cfg.API.Enabled {
		go api.RunService(ctx, Name+"_SERVER", auth)
	}

	redirAddr := tunnel.NewAddressFromHostPort("tcp", cfg.RemoteHost, cfg.RemotePort)
	s := &Server{
		underlay:    underlay,
		auth:        auth,
		redirAddr:   redirAddr,
		connChan:    make(chan tunnel.Conn, 256),
		muxChan:     make(chan tunnel.Conn, 256),
		singMuxChan: make(chan tunnel.Conn, 256),
		muxCoolChan: make(chan tunnel.Conn, 256),
		packetChan:  make(chan tunnel.PacketConn, 256),
		ctx:         ctx,
		cancel:      cancel,
		redir:       redirector.NewRedirector(ctx),
	}

	if !cfg.DisableHTTPCheck {
		redirConn, err := net.Dial("tcp", redirAddr.String())
		if err != nil {
			cancel()
			return nil, common.NewError("invalid redirect address. check your http server: " + redirAddr.String()).Base(err)
		}
		redirConn.Close()
	}

	// Expose accept-channel water-marks to the dashboard.
	connmonitor.GlobalMetrics().RegisterChannelGauge(connmonitor.ChannelGauge{
		Name:  "trojan.connChan",
		Depth: func() int { return len(s.connChan) },
		Cap:   func() int { return cap(s.connChan) },
	})
	connmonitor.GlobalMetrics().RegisterChannelGauge(connmonitor.ChannelGauge{
		Name:  "trojan.muxChan",
		Depth: func() int { return len(s.muxChan) },
		Cap:   func() int { return cap(s.muxChan) },
	})
	connmonitor.GlobalMetrics().RegisterChannelGauge(connmonitor.ChannelGauge{
		Name:  "trojan.singMuxChan",
		Depth: func() int { return len(s.singMuxChan) },
		Cap:   func() int { return cap(s.singMuxChan) },
	})
	connmonitor.GlobalMetrics().RegisterChannelGauge(connmonitor.ChannelGauge{
		Name:  "trojan.packetChan",
		Depth: func() int { return len(s.packetChan) },
		Cap:   func() int { return cap(s.packetChan) },
	})

	go s.acceptLoop()
	log.Debug("trojan server created")
	return s, nil
}
