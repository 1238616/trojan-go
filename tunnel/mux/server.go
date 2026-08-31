package mux

import (
	"context"
	"strings"
	"time"

	"github.com/xtaci/smux"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

// Server is a smux server
type Server struct {
	underlay         tunnel.Server
	connChan         chan tunnel.Conn
	maxStreamBuffer  int // Phase 4: per-stream window
	maxReceiveBuffer int // Phase 4: session-level window
	ctx              context.Context
	cancel           context.CancelFunc
}

func (s *Server) acceptConnWorker() {
	for {
		conn, err := s.underlay.AcceptConn(&Tunnel{})
		if err != nil {
			log.Debug(err)
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			continue
		}
		log.Debug("mux: accepted conn from ", conn.RemoteAddr())
		if m := conn.Metadata(); m != nil && m.Address != nil {
			log.Debug("mux: conn metadata domain=", m.DomainName, " port=", m.Port)
		}
		go func(conn tunnel.Conn) {
			smuxConfig := smux.DefaultConfig()
			// Phase 4: tune smux windows for high-BDP links.
			if s.maxStreamBuffer > 0 {
				sb := s.maxStreamBuffer
				if sb < 64<<10 {
					sb = 64 << 10
				}
				if sb > 64<<20 {
					sb = 64 << 20
				}
				smuxConfig.MaxStreamBuffer = sb
			}
			if s.maxReceiveBuffer > 0 {
				rb := s.maxReceiveBuffer
				if rb < 1<<20 {
					rb = 1 << 20
				}
				if rb > 256<<20 {
					rb = 256 << 20
				}
				smuxConfig.MaxReceiveBuffer = rb
			}
			smuxConfig.KeepAliveInterval = 15 * time.Second
			smuxConfig.KeepAliveTimeout = 60 * time.Second
			smuxSession, err := smux.Server(conn, smuxConfig)
			if err != nil {
				log.Error(err)
				return
			}
			go func(session *smux.Session, conn tunnel.Conn) {
				defer session.Close()
				defer conn.Close()
				for {
					stream, err := session.AcceptStream()
					if err != nil {
						if strings.Contains(err.Error(), "closed pipe") || err.Error() == "EOF" {
							log.Debug("mux session closed: ", err)
						} else {
							log.Error(err)
						}
						return
					}
					select {
					case s.connChan <- &Conn{
						rwc:  stream,
						Conn: conn,
					}:
					case <-s.ctx.Done():
						log.Debug("exiting")
						return
					}
				}
			}(smuxSession, conn)
		}(conn)
	}
}

func (s *Server) AcceptConn(tunnel.Tunnel) (tunnel.Conn, error) {
	select {
	case conn := <-s.connChan:
		return conn, nil
	case <-s.ctx.Done():
		return nil, common.NewError("mux server closed")
	}
}

func (s *Server) AcceptPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	panic("not supported")
}

func (s *Server) Close() error {
	s.cancel()
	return s.underlay.Close()
}

func NewServer(ctx context.Context, underlay tunnel.Server) (*Server, error) {
	cfg := config.FromContext(ctx, Name).(*Config)
	ctx, cancel := context.WithCancel(ctx)
	server := &Server{
		underlay:         underlay,
		ctx:              ctx,
		cancel:           cancel,
		connChan:         make(chan tunnel.Conn, 256),
		maxStreamBuffer:  cfg.Mux.MaxStreamBuffer,
		maxReceiveBuffer: cfg.Mux.MaxReceiveBuffer,
	}
	go server.acceptConnWorker()
	log.Debug("mux server created")
	return server, nil
}
