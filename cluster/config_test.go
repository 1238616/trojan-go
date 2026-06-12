package cluster

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/p4gefau1t/trojan-go/config"
)

func TestConfigDefaults(t *testing.T) {
	// Use WithJSONConfig to trigger init() defaults, then verify via FromContext
	data := []byte(`{"run_type":"server"}`)
	ctx, err := config.WithJSONConfig(context.Background(), data)
	if err != nil {
		t.Fatalf("WithJSONConfig failed: %v", err)
	}

	raw := config.FromContext(ctx, Name)
	tlc, ok := raw.(*TopLevelConfig)
	if !ok {
		t.Fatalf("expected *TopLevelConfig, got %T", raw)
	}
	cfg := &tlc.Cluster

	if cfg.ProbeInterval != 120 {
		t.Fatalf("expected ProbeInterval=120, got %d", cfg.ProbeInterval)
	}
	if cfg.ProbeTimeout != 3000 {
		t.Fatalf("expected ProbeTimeout=3000, got %d", cfg.ProbeTimeout)
	}
	if cfg.RelayThreshold != 50 {
		t.Fatalf("expected RelayThreshold=50, got %d", cfg.RelayThreshold)
	}
	if cfg.LatencyThreshold != 100 {
		t.Fatalf("expected LatencyThreshold=100, got %d", cfg.LatencyThreshold)
	}
	if cfg.NodeName != "local" {
		t.Fatalf("expected NodeName=local, got %q", cfg.NodeName)
	}
}

func TestConfigParsePeers(t *testing.T) {
	data := `{
		"enabled": true,
		"node_name": "singapore-1",
		"probe_interval": 60,
		"probe_timeout": 2000,
		"relay_threshold": 80,
		"latency_threshold": 150,
		"targets": ["cidr:149.154.160.0/20"],
		"peers": [
			{
				"name": "tokyo-1",
				"host": "tokyo.example.com",
				"port": 443,
				"password": "secret",
				"weight": 10,
				"websocket": {"enabled": true, "host": "tokyo.example.com", "path": "/ws"},
				"ssl": {"sni": "tokyo.example.com", "verify": true}
			}
		]
	}`

	var cfg Config
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if !cfg.Enabled {
		t.Fatal("expected Enabled=true")
	}
	if cfg.NodeName != "singapore-1" {
		t.Fatalf("expected NodeName=singapore-1, got %q", cfg.NodeName)
	}
	if cfg.ProbeInterval != 60 {
		t.Fatalf("expected ProbeInterval=60, got %d", cfg.ProbeInterval)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(cfg.Peers))
	}

	peer := cfg.Peers[0]
	if peer.Name != "tokyo-1" {
		t.Fatalf("expected peer name tokyo-1, got %q", peer.Name)
	}
	if !peer.Websocket.Enabled {
		t.Fatal("expected peer websocket enabled")
	}
	if peer.Websocket.Path != "/ws" {
		t.Fatalf("expected ws path /ws, got %q", peer.Websocket.Path)
	}
	if peer.SSL.SNI != "tokyo.example.com" {
		t.Fatalf("expected sni tokyo.example.com, got %q", peer.SSL.SNI)
	}
	if !peer.SSL.Verify {
		t.Fatal("expected ssl verify=true")
	}
}
