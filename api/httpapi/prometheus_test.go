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
		// Phase 3: per-user
		`trojan_user_conns_total{hash="abc123"} 50`,
		`trojan_user_bytes_down_total{hash="abc123"} 99999`,
	}
	for _, w := range wantContains {
		if !strings.Contains(out, w) {
			t.Errorf("Prometheus output missing %q\n--- full output ---\n%s", w, out)
		}
	}

	// Series whose Record* methods had no callers were removed together
	// with their metrics (issue #7): they must not be exported anymore,
	// otherwise dashboards would show permanently-zero counters.
	for _, gone := range []string{
		"trojan_splice_bytes_total", "trojan_splice_calls_total",
		"trojan_splice_fallback_total", "trojan_accept_drops_total",
		"trojan_tcp_rtt_p50_us", "trojan_tcp_rtt_p95_us",
		"trojan_tcp_cwnd_p50", "trojan_tcp_cwnd_p95", "trojan_tcp_loss_total",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("removed series %q must not be exported\n--- full output ---\n%s", gone, out)
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
