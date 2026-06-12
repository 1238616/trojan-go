package httpapi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

func TestEmitPrometheusContainsExpectedSeries(t *testing.T) {
	snap := connmonitor.MetricsSnapshot{
		ConnOpenTotal:            42,
		ConnCloseTotal:           40,
		OpenCPS:                  3,
		CloseCPS:                 2,
		CloseByReason:            map[string]uint64{"eof": 30, "timeout": 10},
		TLSHandshakeTotal:        100,
		TLSHandshakeFailed:       2,
		TLSHandshakeResumedTotal: 50,
		TLSHandshakeP50Ms:        12.5,
		TLSHandshakeP95Ms:        30.1,
		Pools:                    []connmonitor.PoolSample{{Name: "tcp_relay", Gets: 100, News: 5, Puts: 90, HitRate: 0.95}},
		TopTargets:               []connmonitor.TargetSample{{Host: "api.x", Dials: 20, BytesDown: 9999}},
		PacketCloseByReason:      map[string]uint64{},
		OriginDialFailByKind:     map[string]uint64{},
		TrojanAuthFailByKind:     map[string]uint64{},
		// Phase 3 fields
		MuxStreamsActive:     5,
		MuxStreamsTotal:      120,
		MuxStreamsPerConnP50: 3.5,
		MuxStreamsPerConnP95: 8.2,
		SpliceBytesTotal:     1024000,
		SpliceCallsTotal:     500,
		SpliceFallbackTotal:  10,
		TCPRttP50Us:          2500,
		TCPRttP95Us:          8000,
		TCPCwndP50:           100,
		TCPCwndP95:           200,
		TCPLossTotal:         3,
		Users:                []connmonitor.UserSample{{Hash: "abc123", Conns: 50, BytesDown: 99999}},
	}
	var buf bytes.Buffer
	emitPrometheus(&buf, snap)
	out := buf.String()

	wantContains := []string{
		// Phase 1 + 2
		"trojan_conn_open_total 42",
		"trojan_tls_handshake_resumed_total 50",
		`trojan_conn_close_by_reason_total{reason="eof"} 30`,
		`trojan_pool_hit_rate{name="tcp_relay"} 0.95`,
		`trojan_target_bytes_down_total{host="api.x"} 9999`,
		"# HELP trojan_conn_open_total",
		"# TYPE trojan_conn_open_total counter",
		// Phase 3: mux
		"trojan_mux_streams_active 5",
		"trojan_mux_streams_total 120",
		"trojan_mux_streams_per_conn_p50 3.5",
		"trojan_mux_streams_per_conn_p95 8.2",
		// Phase 3: splice
		"trojan_splice_bytes_total 1.024e+06",
		"trojan_splice_calls_total 500",
		"trojan_splice_fallback_total 10",
		// Phase 3: socket telemetry
		"trojan_tcp_rtt_p50_us 2500",
		"trojan_tcp_rtt_p95_us 8000",
		"trojan_tcp_cwnd_p50 100",
		"trojan_tcp_cwnd_p95 200",
		"trojan_tcp_loss_total 3",
		// Phase 3: per-user
		`trojan_user_conns_total{hash="abc123"} 50`,
		`trojan_user_bytes_down_total{hash="abc123"} 99999`,
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("Prometheus output missing %q\n--- full output ---\n%s", w, out)
		}
	}
}

func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		`plain`:         `plain`,
		`with "quote"`:  `with \"quote\"`,
		"real\nnewline": "real\\nnewline",
		"back\\slash":   "back\\\\slash",
	}
	for in, want := range cases {
		got := escapeLabel(in)
		if got != want {
			t.Errorf("escapeLabel(%q)=%q, want %q", in, got, want)
		}
	}
}
