package freedom

import (
	"testing"
)

// TestTCPConfigPhase4Defaults verifies Phase 4 TCP tuning defaults.
func TestTCPConfigPhase4Defaults(t *testing.T) {
	cfg := &Config{
		TCP: TCPConfig{
			PreferIPV4:   false,
			NoDelay:      true,
			KeepAlive:    true,
			DialTimeout:  10,
			KeepIdleSec:  30,
			KeepIntvlSec: 10,
			KeepCnt:      3,
		},
	}
	if cfg.TCP.DialTimeout != 10 {
		t.Errorf("DialTimeout=%d, want 10", cfg.TCP.DialTimeout)
	}
	if cfg.TCP.KeepIdleSec != 30 {
		t.Errorf("KeepIdleSec=%d, want 30", cfg.TCP.KeepIdleSec)
	}
	if cfg.TCP.KeepIntvlSec != 10 {
		t.Errorf("KeepIntvlSec=%d, want 10", cfg.TCP.KeepIntvlSec)
	}
	if cfg.TCP.KeepCnt != 3 {
		t.Errorf("KeepCnt=%d, want 3", cfg.TCP.KeepCnt)
	}
	if cfg.TCP.ReadBuffer != 0 {
		t.Errorf("ReadBuffer=%d, want 0 (OS default)", cfg.TCP.ReadBuffer)
	}
	if cfg.TCP.WriteBuffer != 0 {
		t.Errorf("WriteBuffer=%d, want 0 (OS default)", cfg.TCP.WriteBuffer)
	}
}

// TestTCPConfigBufferValues verifies custom buffer sizes are stored.
func TestTCPConfigBufferValues(t *testing.T) {
	cfg := &Config{
		TCP: TCPConfig{
			ReadBuffer:  4 << 20, // 4 MB
			WriteBuffer: 4 << 20,
		},
	}
	if cfg.TCP.ReadBuffer != 4<<20 {
		t.Errorf("ReadBuffer=%d, want %d", cfg.TCP.ReadBuffer, 4<<20)
	}
	if cfg.TCP.WriteBuffer != 4<<20 {
		t.Errorf("WriteBuffer=%d, want %d", cfg.TCP.WriteBuffer, 4<<20)
	}
}

// TestClientFieldsFromConfig verifies that Client struct fields are
// populated from the config.
func TestClientFieldsFromConfig(t *testing.T) {
	c := &Client{
		readBuffer:   4 << 20,
		writeBuffer:  4 << 20,
		keepIdleSec:  30,
		keepIntvlSec: 10,
		keepCnt:      3,
		dialTimeout:  10,
		noDelay:      true,
		keepAlive:    true,
	}
	if c.dialTimeout != 10 {
		t.Errorf("dialTimeout=%d, want 10", c.dialTimeout)
	}
	if c.readBuffer != 4<<20 {
		t.Errorf("readBuffer=%d, want %d", c.readBuffer, 4<<20)
	}
	if c.writeBuffer != 4<<20 {
		t.Errorf("writeBuffer=%d, want %d", c.writeBuffer, 4<<20)
	}
	if c.keepIdleSec != 30 {
		t.Errorf("keepIdleSec=%d, want 30", c.keepIdleSec)
	}
}
