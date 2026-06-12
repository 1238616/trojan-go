package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
)

func TestConnectionsEndpoint(t *testing.T) {
	monitor := connmonitor.Global()
	monitor.Register("test-1", "example.com:443")
	monitor.RecordUpload("test-1", 100)
	monitor.RecordDownload("test-1", 200)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/connections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetAll())
	})

	req := httptest.NewRequest("GET", "/api/connections", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %s", ct)
	}

	var conns []connmonitor.ConnInfo
	if err := json.Unmarshal(w.Body.Bytes(), &conns); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	found := false
	for _, c := range conns {
		if c.ID == "test-1" {
			found = true
			if c.UploadBytes != 100 {
				t.Fatalf("expected upload 100, got %d", c.UploadBytes)
			}
		}
	}
	if !found {
		t.Fatal("test-1 connection not found in response")
	}

	// Cleanup
	monitor.Unregister("test-1")
}

func TestSummaryEndpoint(t *testing.T) {
	monitor := connmonitor.Global()
	monitor.Register("test-2", "google.com:80")
	monitor.RecordDownload("test-2", 500)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetSummary())
	})

	req := httptest.NewRequest("GET", "/api/summary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var s connmonitor.Summary
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if s.ActiveConnections < 1 {
		t.Fatalf("expected at least 1 active, got %d", s.ActiveConnections)
	}

	monitor.Unregister("test-2")
}

func TestHistoryEndpoint(t *testing.T) {
	monitor := connmonitor.Global()
	monitor.Register("test-3", "history.example.com:443")
	monitor.RecordUpload("test-3", 1000)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetHistory())
	})

	req := httptest.NewRequest("GET", "/api/history", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %s", ct)
	}

	var history []connmonitor.TrafficPoint
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	// History may be empty if calcLoop hasn't ticked yet, just verify it's valid JSON array
	monitor.Unregister("test-3")
}

func TestDashboardEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("expected text/html, got %s", ct)
	}
	body := w.Body.String()
	if len(body) < 100 {
		t.Fatal("dashboard HTML response too short")
	}
	// Verify key elements are present
	for _, needle := range []string{"trafficChart", "chart.js", "fetchAll", "/api/history"} {
		if !contains(body, needle) {
			t.Fatalf("dashboard HTML missing expected content: %s", needle)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Helper: builds a test mux with auth middleware.
func buildAuthMux(secret string) *http.ServeMux {
	monitor := connmonitor.Global()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if secret == "" {
				next(w, r)
				return
			}
			token := ""
			if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
				token = a[7:]
			}
			if token == "" {
				token = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/summary", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor.GetSummary())
	}))
	return mux
}

func TestAuthMiddleware_NoSecret(t *testing.T) {
	mux := buildAuthMux("")
	req := httptest.NewRequest("GET", "/api/summary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("no secret: expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidBearer(t *testing.T) {
	mux := buildAuthMux("mysecret123")
	req := httptest.NewRequest("GET", "/api/summary", nil)
	req.Header.Set("Authorization", "Bearer mysecret123")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("valid bearer: expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidQueryToken(t *testing.T) {
	mux := buildAuthMux("mysecret123")
	req := httptest.NewRequest("GET", "/api/summary?token=mysecret123", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("valid query token: expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	mux := buildAuthMux("mysecret123")
	req := httptest.NewRequest("GET", "/api/summary", nil)
	req.Header.Set("Authorization", "Bearer wrongtoken")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("invalid token: expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_MissingToken(t *testing.T) {
	mux := buildAuthMux("mysecret123")
	req := httptest.NewRequest("GET", "/api/summary", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("missing token: expected 401, got %d", w.Code)
	}
}

func TestDashboardContainsLoginUI(t *testing.T) {
	for _, needle := range []string{"loginOverlay", "secretInput", "doLogin", "doLogout", "/api/auth"} {
		if !contains(dashboardHTML, needle) {
			t.Fatalf("dashboard HTML missing login UI element: %s", needle)
		}
	}
}
