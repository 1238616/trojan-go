package cluster

import "github.com/p4gefau1t/trojan-go/config"

const Name = "CLUSTER"

type PeerWebsocketConfig struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Host    string `json:"host" yaml:"host"`
	Path    string `json:"path" yaml:"path"`
}

type PeerSSLConfig struct {
	SNI string `json:"sni" yaml:"sni"`
	// Verify controls peer certificate verification. It is a pointer so the
	// ABSENT case can be told from an explicit false: absent means verify
	// ENABLED (the safe default, issue #12); only "verify": false disables
	// it. Flipping this default is a deliberate breaking change — configs
	// that relied on the old insecure default must now opt out explicitly.
	Verify *bool `json:"verify" yaml:"verify"`
}

// PeerMuxConfig enables connection multiplexing toward a peer: one
// authenticated trojan tunnel (smux session) carries many relay streams,
// so per-connection TCP+TLS+WS handshakes are amortized away. Requires
// the peer node to run with mux enabled.
type PeerMuxConfig struct {
	Enabled     bool `json:"enabled" yaml:"enabled"`
	Concurrency int  `json:"concurrency" yaml:"concurrency"` // max streams per session; default 8
}

type PeerConfig struct {
	Name      string              `json:"name" yaml:"name"`
	Host      string              `json:"host" yaml:"host"`
	Port      int                 `json:"port" yaml:"port"`
	Password  string              `json:"password" yaml:"password"`
	Weight    int                 `json:"weight" yaml:"weight"`
	Websocket PeerWebsocketConfig `json:"websocket" yaml:"websocket"`
	SSL       PeerSSLConfig       `json:"ssl" yaml:"ssl"`
	Mux       PeerMuxConfig       `json:"mux" yaml:"mux"`
}

type Config struct {
	Enabled          bool         `json:"enabled" yaml:"enabled"`
	ForceRelay       bool         `json:"force_relay" yaml:"force-relay"`
	NodeName         string       `json:"node_name" yaml:"node-name"`
	Peers            []PeerConfig `json:"peers" yaml:"peers"`
	ProbeInterval    int          `json:"probe_interval" yaml:"probe-interval"`
	ProbeTimeout     int          `json:"probe_timeout" yaml:"probe-timeout"`
	RelayThreshold   int          `json:"relay_threshold" yaml:"relay-threshold"`
	LatencyThreshold int          `json:"latency_threshold" yaml:"latency-threshold"`
	Targets          []string     `json:"targets" yaml:"targets"`
}

// TopLevelConfig wraps Config to match the JSON/YAML top-level key "cluster".
type TopLevelConfig struct {
	Cluster Config `json:"cluster" yaml:"cluster"`
}

func init() {
	config.RegisterConfigCreator(Name, func() interface{} {
		return &TopLevelConfig{
			Cluster: Config{
				ProbeInterval:    120,
				ProbeTimeout:     3000,
				RelayThreshold:   50,
				LatencyThreshold: 100,
				NodeName:         "local",
			},
		}
	})
}
