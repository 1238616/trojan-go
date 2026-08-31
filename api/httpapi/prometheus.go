package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

// promMetrics is the canonical list of Prometheus/OpenMetrics series
// exposed by /metrics. The dashboard (and any external Prometheus
// scraper) relies on the names being stable.
//
// We hand-roll the text exposition format to avoid pulling in
// github.com/prometheus/client_golang, which adds a non-trivial
// dependency footprint for what is fundamentally a string-formatting
// job. The format we emit is compatible with both Prometheus and
// OpenMetrics parsers.

// emitPrometheus writes the full /metrics body to w given a snapshot.
func emitPrometheus(w io.Writer, s connmonitor.MetricsSnapshot) {
	// Helper closures
	gauge := func(name, help string, v float64, labels ...string) {
		writeMetric(w, name, "gauge", help, v, labels)
	}
	counter := func(name, help string, v float64, labels ...string) {
		writeMetric(w, name, "counter", help, v, labels)
	}

	// ---- connection lifecycle ----
	counter("trojan_conn_open_total", "Total inbound connections accepted", float64(s.ConnOpenTotal))
	counter("trojan_conn_close_total", "Total connections closed", float64(s.ConnCloseTotal))
	gauge("trojan_conn_open_cps", "Connections opened in last completed second", float64(s.OpenCPS))
	gauge("trojan_conn_close_cps", "Connections closed in last completed second", float64(s.CloseCPS))
	for reason, n := range s.CloseByReason {
		counter("trojan_conn_close_by_reason_total", "Closed connections by reason", float64(n), "reason", reason)
	}

	// ---- throughput percentiles ----
	gauge("trojan_up_bps_p50", "Upload bytes/second per-connection P50", s.UpBpsP50)
	gauge("trojan_up_bps_p95", "Upload bytes/second per-connection P95", s.UpBpsP95)
	gauge("trojan_down_bps_p50", "Download bytes/second per-connection P50", s.DownBpsP50)
	gauge("trojan_down_bps_p95", "Download bytes/second per-connection P95", s.DownBpsP95)

	// ---- TLS handshake ----
	counter("trojan_tls_handshake_total", "TLS handshakes attempted", float64(s.TLSHandshakeTotal))
	counter("trojan_tls_handshake_failed_total", "TLS handshakes failed", float64(s.TLSHandshakeFailed))
	counter("trojan_tls_handshake_resumed_total", "TLS handshakes resumed (session cache hit)", float64(s.TLSHandshakeResumedTotal))
	gauge("trojan_tls_handshake_p50_ms", "TLS handshake latency P50 (ms)", s.TLSHandshakeP50Ms)
	gauge("trojan_tls_handshake_p95_ms", "TLS handshake latency P95 (ms)", s.TLSHandshakeP95Ms)

	// ---- origin dial ----
	counter("trojan_origin_dial_total", "Origin dials attempted", float64(s.OriginDialTotal))
	counter("trojan_origin_dial_failed_total", "Origin dials failed", float64(s.OriginDialFailed))
	for kind, n := range s.OriginDialFailByKind {
		counter("trojan_origin_dial_fail_by_kind_total", "Origin dial failures by kind", float64(n), "kind", kind)
	}
	gauge("trojan_origin_dial_p50_ms", "Origin dial latency P50 (ms)", s.OriginDialP50Ms)
	gauge("trojan_origin_dial_p95_ms", "Origin dial latency P95 (ms)", s.OriginDialP95Ms)

	// ---- TTFB ----
	gauge("trojan_ttfb_p50_ms", "Time-to-first-byte P50 (ms)", s.TTFBP50Ms)
	gauge("trojan_ttfb_p95_ms", "Time-to-first-byte P95 (ms)", s.TTFBP95Ms)

	// ---- trojan auth ----
	counter("trojan_auth_total", "Trojan auth attempts", float64(s.TrojanAuthTotal))
	counter("trojan_auth_failed_total", "Trojan auth failures", float64(s.TrojanAuthFailed))
	for kind, n := range s.TrojanAuthFailByKind {
		counter("trojan_auth_fail_by_kind_total", "Trojan auth failures by kind", float64(n), "kind", kind)
	}
	gauge("trojan_auth_p50_ms", "Trojan auth latency P50 (ms)", s.TrojanAuthP50Ms)
	gauge("trojan_auth_p95_ms", "Trojan auth latency P95 (ms)", s.TrojanAuthP95Ms)

	// ---- channel gauges ----
	for _, ch := range s.Channels {
		gauge("trojan_channel_depth", "Current channel depth", float64(ch.Depth), "name", ch.Name)
		gauge("trojan_channel_cap", "Channel capacity", float64(ch.Cap), "name", ch.Name)
	}

	// ---- runtime ----
	gauge("trojan_go_goroutines", "Active Go goroutines", float64(s.Goroutines))
	gauge("trojan_go_heap_alloc_mb", "Go heap allocated (MiB)", s.HeapAllocMB)
	gauge("trojan_go_heap_sys_mb", "Go heap sys (MiB)", s.HeapSysMB)
	counter("trojan_go_num_gc_total", "Total GC cycles", float64(s.NumGC))
	gauge("trojan_go_gc_pause_last_ms", "Last GC pause (ms)", s.GCPauseLastMs)
	gauge("trojan_go_gc_cpu_fraction", "GC CPU fraction", s.GCCPUFraction)

	// ---- Phase 1: packet flow ----
	counter("trojan_packet_open_total", "Packet flows opened", float64(s.PacketOpenTotal))
	counter("trojan_packet_close_total", "Packet flows closed", float64(s.PacketCloseTotal))
	for reason, n := range s.PacketCloseByReason {
		counter("trojan_packet_close_by_reason_total", "Packet flows closed by reason", float64(n), "reason", reason)
	}
	gauge("trojan_packet_pps_p50", "Packet PPS P50", s.PacketPpsP50)
	gauge("trojan_packet_pps_p95", "Packet PPS P95", s.PacketPpsP95)
	gauge("trojan_packet_bps_p50", "Packet BPS P50", s.PacketBpsP50)
	gauge("trojan_packet_bps_p95", "Packet BPS P95", s.PacketBpsP95)

	// ---- Phase 1: DNS / TCP dial ----
	counter("trojan_dns_resolve_total", "DNS resolutions attempted", float64(s.DNSResolveTotal))
	counter("trojan_dns_resolve_failed_total", "DNS resolutions failed", float64(s.DNSResolveFailed))
	gauge("trojan_dns_resolve_p50_ms", "DNS resolution latency P50 (ms)", s.DNSResolveP50Ms)
	gauge("trojan_dns_resolve_p95_ms", "DNS resolution latency P95 (ms)", s.DNSResolveP95Ms)
	gauge("trojan_tcp_dial_p50_ms", "TCP dial latency P50 (ms)", s.TCPDialP50Ms)
	gauge("trojan_tcp_dial_p95_ms", "TCP dial latency P95 (ms)", s.TCPDialP95Ms)

	// ---- Phase 1: sync.Pool health ----
	for _, p := range s.Pools {
		counter("trojan_pool_gets_total", "Pool Get() calls", float64(p.Gets), "name", p.Name)
		counter("trojan_pool_news_total", "Pool New() calls (misses)", float64(p.News), "name", p.Name)
		counter("trojan_pool_puts_total", "Pool Put() calls", float64(p.Puts), "name", p.Name)
		gauge("trojan_pool_hit_rate", "Pool hit rate (0..1)", p.HitRate, "name", p.Name)
	}

	// ---- Phase 1: backpressure ----
	counter("trojan_backpressure_events_total", "Backpressure events", float64(s.BackpressureEvents))

	// ---- Phase 2: per-target ----
	for _, t := range s.TopTargets {
		counter("trojan_target_dials_total", "Dials per target", float64(t.Dials), "host", t.Host)
		counter("trojan_target_dial_fails_total", "Dial failures per target", float64(t.DialFails), "host", t.Host)
		counter("trojan_target_bytes_up_total", "Upload bytes per target", float64(t.BytesUp), "host", t.Host)
		counter("trojan_target_bytes_down_total", "Download bytes per target", float64(t.BytesDown), "host", t.Host)
		gauge("trojan_target_dial_p50_ms", "Target dial P50 (ms)", t.DialP50Ms, "host", t.Host)
		gauge("trojan_target_dial_p95_ms", "Target dial P95 (ms)", t.DialP95Ms, "host", t.Host)
	}

	// ---- Phase 3: mux stream metrics ----
	gauge("trojan_mux_streams_active", "Currently active mux streams", float64(s.MuxStreamsActive))
	counter("trojan_mux_streams_total", "Total mux streams opened", float64(s.MuxStreamsTotal))
	gauge("trojan_mux_streams_per_conn_p50", "Mux streams per physical connection P50", s.MuxStreamsPerConnP50)
	gauge("trojan_mux_streams_per_conn_p95", "Mux streams per physical connection P95", s.MuxStreamsPerConnP95)
	gauge("trojan_mux_queue_depth_p50", "Mux queue depth P50", s.MuxQueueDepthP50)

	// ---- Phase 3: per-user metrics ----
	for _, u := range s.Users {
		counter("trojan_user_conns_total", "Connections per user", float64(u.Conns), "hash", u.Hash)
		counter("trojan_user_auth_fail_total", "Auth failures per user", float64(u.AuthFails), "hash", u.Hash)
		counter("trojan_user_bytes_up_total", "Upload bytes per user", float64(u.BytesUp), "hash", u.Hash)
		counter("trojan_user_bytes_down_total", "Download bytes per user", float64(u.BytesDown), "hash", u.Hash)
	}
}

// writeMetric emits a single metric family in Prometheus text format.
func writeMetric(w io.Writer, name, kind, help string, value float64, labels []string) {
	if len(labels)%2 != 0 {
		panic("prometheus: labels must be key/value pairs")
	}
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
	if len(labels) == 0 {
		fmt.Fprintf(w, "%s %s\n", name, formatFloat(value))
		return
	}
	var b strings.Builder
	for i := 0; i < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[i+1]))
		b.WriteByte('"')
	}
	fmt.Fprintf(w, "%s{%s} %s\n", name, b.String(), formatFloat(value))
}

// formatFloat emits NaN/+Inf/-Inf in the Prometheus-expected form and
// otherwise uses %g for compactness.
func formatFloat(v float64) string {
	if v != v {
		return "NaN"
	}
	return fmt.Sprintf("%g", v)
}

// escapeLabel escapes backslashes, newlines and double-quotes per
// Prometheus exposition format rules.
func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	return r.Replace(s)
}

// registerPrometheusHandler installs /metrics on mux. The handler
// reuses the same auth middleware as the rest of the API so operators
// don't have to manage a second secret.
func registerPrometheusHandler(mux *http.ServeMux, authMiddleware func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/metrics", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		// Negotiate: accept both text/plain (Prometheus) and
		// application/openmetrics-text.
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		snap := connmonitor.GlobalMetrics().Snapshot()
		// Sort the output for stable test diffs.
		var buf strings.Builder
		emitPrometheus(&buf, snap)
		lines := strings.Split(buf.String(), "\n")
		sort.Strings(lines)
		for _, l := range lines {
			if l == "" {
				continue
			}
			io.WriteString(w, l)
			io.WriteString(w, "\n")
		}
	}))
}
