package singmux

import (
	"context"
	"net"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

type Server struct {
	underlay tunnel.Server
	connChan chan tunnel.Conn
	ctx      context.Context
	cancel   context.CancelFunc
}

func (s *Server) acceptConnWorker() {
	for {
		conn, err := s.underlay.AcceptConn(&Tunnel{})
		if err != nil {
			log.Debug("singmux: accept from underlay: ", err)
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn tunnel.Conn) {
	defer conn.Close()

	log.Info("singmux: handleConn started, reading Request header from ", conn.RemoteAddr())

	req, err := ReadRequest(conn)
	if err != nil {
		log.Error("singmux: read request failed: ", err, " from=", conn.RemoteAddr())
		return
	}
	log.Info("singmux: new session protocol=", ProtocolName(req.Protocol),
		" version=", req.Version, " padding=", req.Padding,
		" from=", conn.RemoteAddr())

	session, err := newServerSession(conn, req)
	if err != nil {
		log.Error("singmux: create session failed: ", err, " protocol=", ProtocolName(req.Protocol))
		return
	}
	log.Info("singmux: session created, accepting streams...")
	defer session.Close()

	for {
		stream, err := session.Accept()
		if err != nil {
			if !session.IsClosed() {
				log.Info("singmux: accept stream error: ", err)
			} else {
				log.Debug("singmux: session closed")
			}
			return
		}
		log.Info("singmux: new stream accepted, reading StreamRequest...")
		go s.handleStream(stream, conn)
	}
}

func (s *Server) handleStream(stream net.Conn, conn tunnel.Conn) {
	streamReq, err := ReadStreamRequest(stream)
	if err != nil {
		log.Error("singmux: read stream request failed: ", err)
		stream.Close()
		return
	}

	log.Info("singmux: stream target=", streamReq.Address, " network=", streamReq.Network)

	if streamReq.Network == "udp" {
		log.Warn("singmux: UDP stream not supported, closing")
		stream.Close()
		return
	}

	meta := &tunnel.Metadata{
		Command: connect,
		Address: streamReq.Address,
	}

	singConn := &SingMuxConn{
		rwc:      newResponseStream(stream),
		Conn:     conn,
		metadata: meta,
	}

	select {
	case s.connChan <- singConn:
	case <-s.ctx.Done():
		stream.Close()
	}
}

func (s *Server) AcceptConn(tunnel.Tunnel) (tunnel.Conn, error) {
	select {
	case conn := <-s.connChan:
		return conn, nil
	case <-s.ctx.Done():
		return nil, common.NewError("singmux server closed")
	}
}

func (s *Server) AcceptPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	<-s.ctx.Done()
	return nil, common.NewError("singmux server closed")
}

func (s *Server) Close() error {
	s.cancel()
	return s.underlay.Close()
}

func NewServer(ctx context.Context, underlay tunnel.Server) (*Server, error) {
	ctx, cancel := context.WithCancel(ctx)
	server := &Server{
		underlay: underlay,
		connChan: make(chan tunnel.Conn, 256),
		ctx:      ctx,
		cancel:   cancel,
	}
	go server.acceptConnWorker()
	log.Debug("singmux server created")
	return server, nil
}
