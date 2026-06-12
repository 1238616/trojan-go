package singmux

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

const (
	ProtocolSmux = 0
	ProtocolYAMux = 1
	ProtocolH2Mux = 2
)

const (
	Version0 = 0
	Version1 = 1
)

const (
	flagUDP  = 1
	flagAddr = 2

	statusSuccess = 0
	statusError   = 1

	connect tunnel.Command = 1
)

// MagicDomain is the destination domain sent by sing-mux clients in the
// trojan metadata to signal a mux connection.
const MagicDomain = "sp.mux.sing-box.arpa"

type Request struct {
	Version  byte
	Protocol byte
	Padding  bool
}

func ReadRequest(reader io.Reader) (*Request, error) {
	var buf [1]byte

	if _, err := io.ReadFull(reader, buf[:]); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	version := buf[0]
	if version > Version1 {
		return nil, fmt.Errorf("unsupported version: %d", version)
	}

	if _, err := io.ReadFull(reader, buf[:]); err != nil {
		return nil, fmt.Errorf("read protocol: %w", err)
	}
	protocol := buf[0]

	var padding bool
	if version == Version1 {
		if _, err := io.ReadFull(reader, buf[:]); err != nil {
			return nil, fmt.Errorf("read padding flag: %w", err)
		}
		padding = buf[0] != 0
		if padding {
			var paddingLen uint16
			if err := binary.Read(reader, binary.BigEndian, &paddingLen); err != nil {
				return nil, fmt.Errorf("read padding length: %w", err)
			}
			if _, err := io.CopyN(io.Discard, reader, int64(paddingLen)); err != nil {
				return nil, fmt.Errorf("skip padding: %w", err)
			}
		}
	}

	return &Request{Version: version, Protocol: protocol, Padding: padding}, nil
}

type StreamRequest struct {
	Network string
	Address *tunnel.Address
}

// ReadStreamRequest reads a sing-mux per-stream request header.
// Format: flags(2 bytes) + socksaddr (same as SOCKS5 address).
func ReadStreamRequest(reader io.Reader) (*StreamRequest, error) {
	var flags uint16
	if err := binary.Read(reader, binary.BigEndian, &flags); err != nil {
		return nil, fmt.Errorf("read flags: %w", err)
	}

	addr := new(tunnel.Address)
	if err := addr.ReadFrom(reader); err != nil {
		return nil, fmt.Errorf("read destination: %w", err)
	}

	network := "tcp"
	if flags&flagUDP != 0 {
		network = "udp"
	}
	addr.NetworkType = network

	return &StreamRequest{Network: network, Address: addr}, nil
}

func ProtocolName(p byte) string {
	switch p {
	case ProtocolSmux:
		return "smux"
	case ProtocolYAMux:
		return "yamux"
	case ProtocolH2Mux:
		return "h2mux"
	default:
		return fmt.Sprintf("unknown(%d)", p)
	}
}
