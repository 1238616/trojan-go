package mux

import "github.com/p4gefau1t/trojan-go/config"

type MuxConfig struct {
	Enabled          bool `json:"enabled" yaml:"enabled"`
	IdleTimeout      int  `json:"idle_timeout" yaml:"idle-timeout"`
	Concurrency      int  `json:"concurrency" yaml:"concurrency"`
	MaxPhysicalConns int  `json:"max_physical_conns" yaml:"max-physical-conns"` // Phase 3: adaptive limit

	// Phase 4: smux window / buffer tuning
	MaxStreamBuffer  int `json:"max_stream_buffer" yaml:"max-stream-buffer"`   // per-stream recv window; bytes; default 1 MB
	MaxReceiveBuffer int `json:"max_receive_buffer" yaml:"max-receive-buffer"` // session-level recv window; bytes; default 16 MB
}

type Config struct {
	Mux MuxConfig `json:"mux" yaml:"mux"`
}

func init() {
	config.RegisterConfigCreator(Name, func() interface{} {
		return &Config{
			Mux: MuxConfig{
				Enabled:          false,
				IdleTimeout:      30,
				Concurrency:      4,
				MaxPhysicalConns: 16,
				// Phase 4: larger windows for high-BDP links
				MaxStreamBuffer:  1 << 20, // 1 MB (smux default 64 KB)
				MaxReceiveBuffer: 1 << 24, // 16 MB (smux default 4 MB)
			},
		}
	})
}
