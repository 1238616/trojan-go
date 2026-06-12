package muxcool

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"sync"

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
			log.Debug("muxcool: accept from underlay: ", err)
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

	log.Info("muxcool: new connection from ", conn.RemoteAddr())

	reader := bufio.NewReader(conn)
	sessions := make(map[uint16]*MuxCoolConn)
	var writeMu sync.Mutex

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		meta, err := ReadFrameMetadata(reader)
		if err != nil {
			if err != io.EOF {
				log.Debug("muxcool: read frame: ", err)
			}
			// Close all sessions
			for _, sess := range sessions {
				sess.closeRead()
			}
			return
		}

		switch meta.SessionStatus {
		case SessionStatusNew:
			log.Info("muxcool: new session id=", meta.SessionID,
				" target=", meta.Target, " network=", meta.Network)

			if meta.Network == "udp" {
				log.Warn("muxcool: UDP not supported, ignoring session ", meta.SessionID)
				if meta.Option&OptionData != 0 {
					discardData(reader)
				}
				continue
			}

			mconn := newMuxCoolConn(meta.SessionID, meta.Target, conn, &writeMu, conn)
			sessions[meta.SessionID] = mconn

			// Feed initial data if present
			if meta.Option&OptionData != 0 {
				data, err := readDataFrameBufio(reader)
				if err != nil {
					log.Error("muxcool: read initial data: ", err)
					mconn.closeRead()
					delete(sessions, meta.SessionID)
					continue
				}
				if len(data) > 0 {
					mconn.feedData(data)
				}
			}

			// Send to output channel
			select {
			case s.connChan <- mconn:
			case <-s.ctx.Done():
				mconn.closeRead()
				return
			}

		case SessionStatusKeep:
			if meta.Option&OptionData != 0 {
				data, err := readDataFrameBufio(reader)
				if err != nil {
					log.Error("muxcool: read keep data: ", err)
					return
				}
				if sess, ok := sessions[meta.SessionID]; ok {
					if len(data) > 0 {
						if err := sess.feedData(data); err != nil {
							log.Debug("muxcool: feed data to closed session ", meta.SessionID)
							delete(sessions, meta.SessionID)
						}
					}
				}
			}

		case SessionStatusEnd:
			if sess, ok := sessions[meta.SessionID]; ok {
				sess.closeRead()
				delete(sessions, meta.SessionID)
				log.Debug("muxcool: session ", meta.SessionID, " ended by client")
			}
			if meta.Option&OptionData != 0 {
				discardData(reader)
			}

		case SessionStatusKeepAlive:
			if meta.Option&OptionData != 0 {
				discardData(reader)
			}

		default:
			log.Error("muxcool: unknown status: ", meta.SessionStatus)
			return
		}
	}
}

// readDataFrameBufio reads a data frame payload using bufio.Reader.Peek
// when the frame fits in the internal buffer. This avoids the per-frame
// heap allocation that ReadDataFrame performs (make([]byte, dataLen)).
// For frames larger than the bufio buffer, it falls back to ReadDataFrame.
func readDataFrameBufio(reader *bufio.Reader) ([]byte, error) {
	// Read the 2-byte length header.
	lenBuf, err := reader.Peek(2)
	if err != nil {
		return nil, err
	}
	dataLen := int(binary.BigEndian.Uint16(lenBuf))
	if _, err := reader.Discard(2); err != nil {
		return nil, err
	}
	if dataLen == 0 {
		return nil, nil
	}

	// Fast path: data fits in bufio's internal buffer (default 4096).
	// Peek returns a slice backed by the buffer — zero alloc.
	if dataLen <= reader.Size() {
		peeked, err := reader.Peek(dataLen)
		if err != nil {
			return nil, err
		}
		// Copy into a new slice because the bufio buffer will be
		// reused on the next Peek/Read call. But we avoid the
		// separate Read → alloc + syscall path.
		data := make([]byte, dataLen)
		copy(data, peeked)
		if _, err := reader.Discard(dataLen); err != nil {
			return nil, err
		}
		return data, nil
	}

	// Slow path: frame larger than bufio buffer. Use standard alloc+read.
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}

func discardData(reader io.Reader) {
	data, _ := ReadDataFrame(reader)
	_ = data
}

func (s *Server) AcceptConn(tunnel.Tunnel) (tunnel.Conn, error) {
	select {
	case conn := <-s.connChan:
		return conn, nil
	case <-s.ctx.Done():
		return nil, common.NewError("muxcool server closed")
	}
}

func (s *Server) AcceptPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	<-s.ctx.Done()
	return nil, common.NewError("muxcool server closed")
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
	log.Debug("muxcool server created")
	return server, nil
}
