package proxy

import "github.com/p4gefau1t/trojan-go/config"

// Config is the process-wide proxy config. Phase 1 adds a handful of
// tuning knobs that are consulted once at startup and then consulted by
// the hot path only through cached values (see SetRelayBufferSize).
//
// JSON/YAML keys follow the existing snake-case / kebab-case convention
// used by the rest of the project.
type Config struct {
	RunType  string `json:"run_type" yaml:"run-type"`
	LogLevel int    `json:"log_level" yaml:"log-level"`
	LogFile  string `json:"log_file" yaml:"log-file"`

	// ---- Phase 1 tuning ----

	// RelayBufferSize overrides the default TCP relay buffer size
	// (default 32 KiB). Clamped to [4 KiB, 1 MiB] by SetRelayBufferSize.
	// Zero / unset keeps the default.
	RelayBufferSize int `json:"relay_buffer_size" yaml:"relay-buffer-size"`

	// BackpressureThresh is the fractional channel fill level (0, 1]
	// above which a backpressure event is recorded in the dashboard.
	// Zero disables back-pressure accounting.
	BackpressureThresh float64 `json:"backpressure_thresh" yaml:"backpressure-thresh"`

	// EnablePacketPool turns on the UDP packet-buffer pool. It is
	// enabled by default; the knob exists so operators can disable it
	// for debugging without rebuilding.
	EnablePacketPool *bool `json:"enable_packet_pool" yaml:"enable-packet-pool"`

	// ---- Phase 3 tuning ----

	// EnableZeroCopy turns on the Linux splice(2) fast path for TCP
	// relays when both sides are plain *net.TCPConn. It is disabled
	// by default because splice bypasses the countingReader, so byte
	// accounting is lost unless the proxy is running without the
	// connection monitor.
	EnableZeroCopy bool `json:"enable_zero_copy" yaml:"enable-zero-copy"`

	// ---- Phase 4 tuning ----

	// GOGC overrides the Go runtime GC target percentage.
	// 0 means "use Go default (100)". Values 100-400 are reasonable
	// for long-lived proxy processes to reduce GC frequency.
	GOGC int `json:"gogc" yaml:"gogc"`

	// MemLimitMB sets a soft memory limit via debug.SetMemoryLimit.
	// 0 means disabled. Unit: megabytes.
	MemLimitMB int64 `json:"mem_limit_mb" yaml:"mem-limit-mb"`

	// ---- Debug / Profiling ----

	// Debug enables the diagnostic profiler. When true, the proxy
	// periodically writes performance snapshots (goroutine count,
	// active connections, GC stats, channel fill, TTFB distribution,
	// latency samples) to the profile_debug/ directory.
	Debug bool `json:"debug" yaml:"debug"`
}

func init() {
	config.RegisterConfigCreator(Name, func() interface{} {
		return &Config{
			LogLevel: 1,
		}
	})
}
