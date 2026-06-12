package singmux

import (
	"fmt"
	"io"
	"net"

	"github.com/hashicorp/yamux"
	"github.com/xtaci/smux"
)

type abstractSession interface {
	Accept() (net.Conn, error)
	Close() error
	IsClosed() bool
	NumStreams() int
}

func newServerSession(conn net.Conn, req *Request) (abstractSession, error) {
	switch req.Protocol {
	case ProtocolSmux:
		cfg := smux.DefaultConfig()
		cfg.KeepAliveDisabled = true
		session, err := smux.Server(conn, cfg)
		if err != nil {
			return nil, err
		}
		return &smuxSession{session}, nil
	case ProtocolYAMux:
		cfg := yamux.DefaultConfig()
		cfg.LogOutput = io.Discard
		session, err := yamux.Server(conn, cfg)
		if err != nil {
			return nil, err
		}
		return &yamuxSession{session}, nil
	case ProtocolH2Mux:
		return nil, fmt.Errorf("h2mux protocol not yet supported")
	default:
		return nil, fmt.Errorf("unknown protocol: %d", req.Protocol)
	}
}

// smuxSession adapts *smux.Session to abstractSession.
type smuxSession struct {
	*smux.Session
}

func (s *smuxSession) Accept() (net.Conn, error) {
	stream, err := s.Session.AcceptStream()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

// yamuxSession adapts *yamux.Session to abstractSession.
type yamuxSession struct {
	*yamux.Session
}
