package cluster

import "github.com/p4gefau1t/trojan-go/config"

const Name = "CLUSTER"

type PeerWebsocketConfig struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Host    string `json:"host" yaml:"host"`
	Path    string `json:"path" yaml:"path"`
}

type PeerSSLConfig struct {
	SNI    string `json:"sni" yaml:"sni"`
	Verify bool   `json:"verify" yaml:"verify"`
}

type PeerConfig struct {
	Name      string              `json:"name" yaml:"name"`
	Host      string              `json:"host" yaml:"host"`
	Port      int                 `json:"port" yaml:"port"`
	Password  string              `json:"password" yaml:"password"`
	Weight    int                 `json:"weight" yaml:"weight"`
	Websocket PeerWebsocketConfig `json:"websocket" yaml:"websocket"`
	SSL       PeerSSLConfig       `json:"ssl" yaml:"ssl"`
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
