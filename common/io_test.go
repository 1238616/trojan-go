package common

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/v2fly/v2ray-core/v4/common"
)

func TestBufferedReader(t *testing.T) {
	payload := [1024]byte{}
	rand.Reader.Read(payload[:])
	rawReader := bytes.NewBuffer(payload[:])
	r := RewindReader{
		rawReader: rawReader,
	}
	r.SetBufferSize(2048)
	buf1 := make([]byte, 512)
	buf2 := make([]byte, 512)
	common.Must2(r.Read(buf1))
	r.Rewind()
	common.Must2(r.Read(buf2))
	if !bytes.Equal(buf1, buf2) {
		t.Fail()
	}
	buf3 := make([]byte, 512)
	common.Must2(r.Read(buf3))
	if !bytes.Equal(buf3, payload[512:]) {
		t.Fail()
	}
	r.Rewind()
	buf4 := make([]byte, 1024)
	common.Must2(r.Read(buf4))
	if !bytes.Equal(payload[:], buf4) {
		t.Fail()
	}
}

func TestRewindReaderPassthrough(t *testing.T) {
	payload := make([]byte, 4096)
	rand.Reader.Read(payload)
	rawReader := bytes.NewBuffer(append([]byte{}, payload...))

	r := &RewindReader{rawReader: rawReader}

	// Before buffering is enabled, passthrough should be false.
	if r.passthrough.Load() {
		t.Fatal("passthrough should be false initially")
	}

	// Enable buffering, read some data, then stop buffering.
	r.SetBufferSize(1024)
	buf := make([]byte, 256)
	n, err := r.Read(buf)
	if err != nil || n != 256 {
		t.Fatalf("expected 256 bytes, got %d, err=%v", n, err)
	}

	// StopBuffering should set passthrough = true (no rewind pending).
	r.StopBuffering()
	if !r.passthrough.Load() {
		t.Fatal("passthrough should be true after StopBuffering with no rewind")
	}

	// Reads should still work correctly through the fast path.
	buf2 := make([]byte, 256)
	n, err = r.Read(buf2)
	if err != nil || n != 256 {
		t.Fatalf("expected 256 bytes on fast path, got %d, err=%v", n, err)
	}
	if !bytes.Equal(buf2, payload[256:512]) {
		t.Fatal("data mismatch on fast-path read")
	}
}

func TestRewindReaderPassthroughResetOnRewind(t *testing.T) {
	payload := make([]byte, 1024)
	rand.Reader.Read(payload)
	rawReader := bytes.NewBuffer(append([]byte{}, payload...))

	r := &RewindReader{rawReader: rawReader}
	r.SetBufferSize(512)

	buf := make([]byte, 128)
	r.Read(buf)

	// Rewind should clear passthrough.
	r.Rewind()
	if r.passthrough.Load() {
		t.Fatal("passthrough should be false after Rewind")
	}

	// Read the rewound data.
	buf2 := make([]byte, 128)
	r.Read(buf2)
	if !bytes.Equal(buf, buf2) {
		t.Fatal("rewound data mismatch")
	}

	// Now StopBuffering.
	r.StopBuffering()
	if !r.passthrough.Load() {
		t.Fatal("passthrough should be true after StopBuffering post-rewind-drain")
	}
}

func TestRewindReaderSetBufferSizeZeroPassthrough(t *testing.T) {
	payload := make([]byte, 512)
	rand.Reader.Read(payload)
	rawReader := bytes.NewBuffer(append([]byte{}, payload...))

	r := &RewindReader{rawReader: rawReader}
	r.SetBufferSize(256)

	buf := make([]byte, 64)
	r.Read(buf)

	// SetBufferSize(0) should enable passthrough when no rewind pending.
	r.SetBufferSize(0)
	if !r.passthrough.Load() {
		t.Fatal("passthrough should be true after SetBufferSize(0)")
	}

	// Re-enabling buffering should clear passthrough.
	r.SetBufferSize(128)
	if r.passthrough.Load() {
		t.Fatal("passthrough should be false after re-enabling buffering")
	}
}
