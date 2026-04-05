package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/app"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestAdminCORSRejectsCrossOriginByDefault(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, config.Config{
		BindAddress:           ":0",
		StatePath:             filepath.Join(t.TempDir(), "gateway.db"),
		AdminToken:            "admin-token",
		GatewayAllowedOrigins: []string{"*"},
	})
	defer server.Close()

	req, err := http.NewRequest(http.MethodOptions, server.URL+"/api/admin/meta", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden preflight, got %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("did not expect admin CORS allow header, got %q", got)
	}
}

func TestGatewayCORSAllowsWildcardOrigins(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, config.Config{
		BindAddress:           ":0",
		StatePath:             filepath.Join(t.TempDir(), "gateway.db"),
		AdminToken:            "admin-token",
		GatewayAllowedOrigins: []string{"*"},
	})
	defer server.Close()

	req, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://console.example")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("expected no content preflight, got %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected wildcard gateway CORS header, got %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-Codex-Turn-State") {
		t.Fatalf("expected Codex turn-state header to be allowed, got %q", got)
	}
}

func TestRealtimeCORSUsesRealtimeOrigins(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, config.Config{
		BindAddress:            ":0",
		StatePath:              filepath.Join(t.TempDir(), "gateway.db"),
		AdminToken:             "admin-token",
		GatewayAllowedOrigins:  []string{"https://gateway.example"},
		RealtimeAllowedOrigins: []string{"https://console.example"},
		EnableRealtime:         true,
	})
	defer server.Close()

	disallowedReq, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/realtime", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	disallowedReq.Header.Set("Origin", "https://gateway.example")

	disallowedRes, err := http.DefaultClient.Do(disallowedReq)
	if err != nil {
		t.Fatalf("do disallowed request: %v", err)
	}
	defer disallowedRes.Body.Close()

	if got := disallowedRes.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("did not expect gateway origin to be allowed on realtime endpoint, got %q", got)
	}

	allowedReq, err := http.NewRequest(http.MethodOptions, server.URL+"/v1/realtime", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	allowedReq.Header.Set("Origin", "https://console.example")

	allowedRes, err := http.DefaultClient.Do(allowedReq)
	if err != nil {
		t.Fatalf("do allowed request: %v", err)
	}
	defer allowedRes.Body.Close()

	if got := allowedRes.Header.Get("Access-Control-Allow-Origin"); got != "https://console.example" {
		t.Fatalf("expected realtime origin to be echoed, got %q", got)
	}
}

func TestHealthzReturnsStructuredStatus(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, config.Config{
		BindAddress: ":0",
		StatePath:   filepath.Join(t.TempDir(), "gateway.db"),
		AdminToken:  "admin-token",
	})
	defer server.Close()

	res, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected healthz ok, got %d", res.StatusCode)
	}

	var payload struct {
		Status        string `json:"status"`
		DBStatus      string `json:"db_status"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		Counts        struct {
			GatewayKeysActive int `json:"gateway_keys_active"`
			Providers         int `json:"providers"`
			Routes            int `json:"routes"`
		} `json:"counts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode healthz payload: %v", err)
	}
	if payload.Status != "ok" || payload.DBStatus != "ok" {
		t.Fatalf("unexpected healthz payload: %+v", payload)
	}
	if payload.UptimeSeconds < 0 {
		t.Fatalf("expected non-negative uptime, got %d", payload.UptimeSeconds)
	}
}

func TestAdminStatsRouteRegistered(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, config.Config{
		BindAddress: ":0",
		StatePath:   filepath.Join(t.TempDir(), "gateway.db"),
		AdminToken:  "admin-token",
	})
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/admin/stats/summary?window=today", nil)
	if err != nil {
		t.Fatalf("new stats request: %v", err)
	}
	req.Header.Set("X-Admin-Token", "admin-token")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stats request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected stats route to be registered, got %d", res.StatusCode)
	}
}

func newTestServer(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	data, err := json.Marshal(model.DefaultState())
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(cfg.StatePath, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	instance, err := app.New(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return httptest.NewServer(instance.Handler())
}
