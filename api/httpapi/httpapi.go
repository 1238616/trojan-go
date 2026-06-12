package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/p4gefau1t/trojan-go/cluster"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

const Name = "CONNMONITOR"

// Config for the HTTP stats API.
type Config struct {
	ConnMonitor ConnMonitorConfig `json:"conn_monitor" yaml:"conn-monitor"`
}

type ConnMonitorConfig struct {
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Addr    string `json:"addr" yaml:"addr"`
	Port    int    `json:"port" yaml:"port"`
	Secret  string `json:"secret" yaml:"secret"`
}

func init() {
	config.RegisterConfigCreator(Name, func() interface{} {
		return &Config{
			ConnMonitor: ConnMonitorConfig{
				Addr: "127.0.0.1",
				Port: 9090,
			},
		}
	})
}

// RunHTTPAPI starts the HTTP stats server if enabled in config.
func RunHTTPAPI(ctx context.Context) error {
	cfg := config.FromContext(ctx, Name).(*Config)
	if !cfg.ConnMonitor.Enabled {
		log.Debug("conn monitor HTTP API disabled")
		return nil
	}

	monitor := connmonitor.Global()
	addr := fmt.Sprintf("%s:%d", cfg.ConnMonitor.Addr, cfg.ConnMonitor.Port)
	secret := cfg.ConnMonitor.Secret

	// authMiddleware checks Bearer token or ?token= query parameter.
	authMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				next(w, r)
				return
			}
			token := ""
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				token = auth[7:]
			}
			if token == "" {
				token = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/connections", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetAll())
	}))
	mux.HandleFunc("/api/summary", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetSummary())
	}))
	mux.HandleFunc("/api/history", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetHistory())
	}))
	// /api/metrics exposes the ROI-Top-5 dashboard signals: connection
	// open/close rates with reason breakdown, throughput percentiles,
	// TLS handshake stats, channel water-marks, and Go runtime numbers.
	mux.HandleFunc("/api/metrics", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(connmonitor.GlobalMetrics().Snapshot())
	}))
	// POST /api/auth verifies the secret and returns success/failure.
	mux.HandleFunc("/api/auth", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if secret == "" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		var body struct {
			Secret string `json:"secret"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if subtle.ConstantTimeCompare([]byte(body.Secret), []byte(secret)) == 1 {
			w.Write([]byte(`{"ok":true}`))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"ok":false,"error":"invalid secret"}`))
		}
	})
	// Dashboard page is always served (auth is handled client-side via login screen).
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})
	// /api/cluster exposes the cluster routing optimization state.
	mux.HandleFunc("/api/cluster", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cr := cluster.GlobalRouter()
		if cr == nil {
			w.Write([]byte(`{"enabled":false}`))
			return
		}
		json.NewEncoder(w).Encode(cr.Snapshot())
	}))
	// Phase 2: /metrics endpoint for Prometheus scrapers. Shares the
	// existing auth middleware so operators don't have to manage a
	// second secret.
	registerPrometheusHandler(mux, authMiddleware)

	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		server.Close()
		monitor.Stop()
	}()

	log.Info("conn monitor HTTP API listening on ", addr)
	log.Info("dashboard available at http://", addr, "/dashboard")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
