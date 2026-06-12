package muxcool

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// --- WriteFrameKeep ---

func BenchmarkWriteFrameKeep(b *testing.B) {
	data := make([]byte, 8*1024) // typical 8KB mux chunk
	var dst bytes.Buffer
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst.Reset()
		_ = WriteFrameKeep(&dst, 42, data)
	}
}

func BenchmarkWriteFrameKeepSizes(b *testing.B) {
	for _, size := range []int{64, 512, 2048, 8192} {
		data := make([]byte, size)
		b.Run("size="+itoa(size), func(b *testing.B) {
			var dst bytes.Buffer
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				dst.Reset()
				_ = WriteFrameKeep(&dst, 42, data)
			}
		})
	}
}

// --- ReadDataFrame ---

func BenchmarkReadDataFrame(b *testing.B) {
	// Prepare a frame: 2-byte length + 1KB payload
	payload := make([]byte, 1024)
	var frame bytes.Buffer
	binary.Write(&frame, binary.BigEndian, uint16(len(payload)))
	frame.Write(payload)
	raw := frame.Bytes()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := bytes.NewReader(raw)
		_, _ = ReadDataFrame(r)
	}
}

// --- ReadFrameMetadata ---

func BenchmarkReadFrameMetadata(b *testing.B) {
	// Build a Keep frame metadata (4 bytes: sessionID + status + option)
	var frame bytes.Buffer
	binary.Write(&frame, binary.BigEndian, uint16(4)) // metaLen
	binary.Write(&frame, binary.BigEndian, uint16(1)) // sessionID
	frame.WriteByte(byte(SessionStatusKeep))          // status
	frame.WriteByte(0)                                // option
	raw := frame.Bytes()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := bytes.NewReader(raw)
		_, _ = ReadFrameMetadata(r)
	}
}

// --- ReadFrameMetadata (New session with address) ---

func BenchmarkReadFrameMetadataNew(b *testing.B) {
	// Build a New frame: sessionID(2) + status(1) + option(1) + network(1) + port(2) + addrType(1) + IPv4(4) = 12
	meta := []byte{
		0x00, 0x01, // sessionID = 1
		byte(SessionStatusNew), // status
		OptionData,             // option
		NetworkTCP,             // network
		0x01, 0xBB,             // port = 443
		0x01,       // addrType = IPv4
		8, 8, 8, 8, // IP = 8.8.8.8
	}
	var frame bytes.Buffer
	binary.Write(&frame, binary.BigEndian, uint16(len(meta)))
	frame.Write(meta)
	raw := frame.Bytes()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := bytes.NewReader(raw)
		_, _ = ReadFrameMetadata(r)
	}
}

// --- WriteFrameEnd ---

func BenchmarkWriteFrameEnd(b *testing.B) {
	var dst bytes.Buffer
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst.Reset()
		_ = WriteFrameEnd(&dst, 42)
	}
}

// --- helpers ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 5)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
