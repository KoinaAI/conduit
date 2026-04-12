package app_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/app"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/store"
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

func newTestServer(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	stateStore, err := store.Open(cfg.StatePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := stateStore.Replace(model.DefaultState()); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	instance, err := app.New(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return httptest.NewServer(instance.Handler())
}
