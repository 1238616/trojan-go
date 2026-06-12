package muxcool

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

type SessionStatus byte

const (
	SessionStatusNew       SessionStatus = 0x01
	SessionStatusKeep      SessionStatus = 0x02
	SessionStatusEnd       SessionStatus = 0x03
	SessionStatusKeepAlive SessionStatus = 0x04
)

const (
	OptionData  byte = 0x01
	OptionError byte = 0x02
)

const (
	NetworkTCP byte = 0x01
	NetworkUDP byte = 0x02
)

// v2ray address types (different from SOCKS5!)
const (
	addrTypeIPv4   byte = 0x01
	addrTypeDomain byte = 0x02
	addrTypeIPv6   byte = 0x03
)

type FrameMetadata struct {
	SessionID     uint16
	SessionStatus SessionStatus
	Option        byte
	Network       string
	Target        *tunnel.Address
}

// ReadFrameMetadata reads a mux.cool frame metadata from the reader.
// Format: metadataLen(2B) + sessionID(2B) + status(1B) + option(1B)
// If status==New: + network(1B) + port(2B) + address
//
// perf: uses stack-allocated arrays and io.ReadFull instead of binary.Read
// to avoid interface-boxing allocations (binary.BigEndian → io.ByteReader).
func ReadFrameMetadata(reader io.Reader) (*FrameMetadata, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
		return nil, err
	}
	metaLen := binary.BigEndian.Uint16(lenBuf[:])
	if metaLen > 512 {
		return nil, fmt.Errorf("invalid metadata length: %d", metaLen)
	}

	var metaBuf [512]byte
	if _, err := io.ReadFull(reader, metaBuf[:metaLen]); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	if metaLen < 4 {
		return nil, fmt.Errorf("metadata too short: %d", metaLen)
	}

	f := &FrameMetadata{
		SessionID:     binary.BigEndian.Uint16(metaBuf[0:2]),
		SessionStatus: SessionStatus(metaBuf[2]),
		Option:        metaBuf[3],
	}

	if f.SessionStatus == SessionStatusNew {
		if int(metaLen) < 5 {
			return nil, fmt.Errorf("metadata too short for New frame")
		}
		network := metaBuf[4]
		switch network {
		case NetworkTCP:
			f.Network = "tcp"
		case NetworkUDP:
			f.Network = "udp"
		default:
			return nil, fmt.Errorf("unknown network: %d", network)
		}

		// v2ray format: PortThenAddress
		// port(2B) + addressType(1B) + address
		remaining := metaBuf[5:metaLen]
		addr, err := parseV2rayAddress(remaining)
		if err != nil {
			return nil, fmt.Errorf("parse address: %w", err)
		}
		addr.NetworkType = f.Network
		f.Target = addr
	}

	return f, nil
}

// parseV2rayAddress parses v2ray's PortThenAddress format.
// Format: port(2B) + addrType(1B) + addr
func parseV2rayAddress(data []byte) (*tunnel.Address, error) {
	if len(data) < 4 { // 2(port) + 1(type) + 1(min addr)
		return nil, fmt.Errorf("address data too short: %d", len(data))
	}

	port := int(binary.BigEndian.Uint16(data[0:2]))
	addrType := data[2]
	addrData := data[3:]

	switch addrType {
	case addrTypeIPv4:
		if len(addrData) < 4 {
			return nil, fmt.Errorf("IPv4 address too short")
		}
		ip := net.IP(addrData[:4])
		return &tunnel.Address{
			IP:          ip,
			Port:        port,
			AddressType: tunnel.IPv4,
		}, nil
	case addrTypeDomain:
		if len(addrData) < 1 {
			return nil, fmt.Errorf("domain length missing")
		}
		domainLen := int(addrData[0])
		if len(addrData) < 1+domainLen {
			return nil, fmt.Errorf("domain data too short")
		}
		domain := string(addrData[1 : 1+domainLen])
		return &tunnel.Address{
			DomainName:  domain,
			Port:        port,
			AddressType: tunnel.DomainName,
		}, nil
	case addrTypeIPv6:
		if len(addrData) < 16 {
			return nil, fmt.Errorf("IPv6 address too short")
		}
		ip := net.IP(addrData[:16])
		return &tunnel.Address{
			IP:          ip,
			Port:        port,
			AddressType: tunnel.IPv6,
		}, nil
	default:
		return nil, fmt.Errorf("unknown address type: %d", addrType)
	}
}

// ReadDataFrame reads the data payload following a frame metadata (when OptionData is set).
// Stream transfer: length(2B) + data
//
// perf: uses stack [2]byte + io.ReadFull instead of binary.Read to avoid
// interface-boxing allocation of binary.BigEndian.
func ReadDataFrame(reader io.Reader) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
		return nil, err
	}
	dataLen := binary.BigEndian.Uint16(lenBuf[:])
	if dataLen == 0 {
		return nil, nil
	}
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}

// WriteFrameKeep writes a Keep frame with data to the writer.
// Uses net.Buffers (writev) to send header+data in a single syscall.
func WriteFrameKeep(writer io.Writer, sessionID uint16, data []byte) error {
	var header [2 + 4 + 2]byte                 // metaLen + metadata + dataLen
	binary.BigEndian.PutUint16(header[0:2], 4) // metaLen = 4
	binary.BigEndian.PutUint16(header[2:4], sessionID)
	header[4] = byte(SessionStatusKeep)
	header[5] = OptionData
	binary.BigEndian.PutUint16(header[6:8], uint16(len(data)))

	bufs := net.Buffers{header[:], data}
	_, err := bufs.WriteTo(writer)
	return err
}

// WriteFrameEnd writes a session End frame.
func WriteFrameEnd(writer io.Writer, sessionID uint16) error {
	var buf [2 + 4]byte // metaLen + metadata
	binary.BigEndian.PutUint16(buf[0:2], 4)
	binary.BigEndian.PutUint16(buf[2:4], sessionID)
	buf[4] = byte(SessionStatusEnd)
	buf[5] = 0
	_, err := writer.Write(buf[:])
	return err
}
