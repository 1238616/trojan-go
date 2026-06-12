package proxy

import (
	"encoding/json"
	"testing"
)

// TestPhase4ConfigFields verifies GOGC and MemLimitMB fields exist
// and have correct zero defaults.
func TestPhase4ConfigFields(t *testing.T) {
	cfg := &Config{
		LogLevel:   1,
		GOGC:       200,
		MemLimitMB: 512,
	}
	if cfg.GOGC != 200 {
		t.Errorf("GOGC=%d, want 200", cfg.GOGC)
	}
	if cfg.MemLimitMB != 512 {
		t.Errorf("MemLimitMB=%d, want 512", cfg.MemLimitMB)
	}
}

// TestPhase4ConfigZeroDefaults verifies that zero values mean "disabled".
func TestPhase4ConfigZeroDefaults(t *testing.T) {
	cfg := &Config{LogLevel: 1}
	if cfg.GOGC != 0 {
		t.Errorf("GOGC=%d, want 0 (disabled)", cfg.GOGC)
	}
	if cfg.MemLimitMB != 0 {
		t.Errorf("MemLimitMB=%d, want 0 (disabled)", cfg.MemLimitMB)
	}
}

func TestDebugConfigFromJSON(t *testing.T) {
	data := []byte(`{"run_type":"server","debug":true}`)
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.Debug {
		t.Fatal("debug should be true after JSON unmarshal with debug:true")
	}

	data2 := []byte(`{"run_type":"server"}`)
	cfg2 := &Config{}
	if err := json.Unmarshal(data2, cfg2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg2.Debug {
		t.Fatal("debug should be false by default")
	}
}
