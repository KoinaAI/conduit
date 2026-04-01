package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/example/universal-ai-gateway/internal/app"
	"github.com/example/universal-ai-gateway/internal/config"
	"github.com/example/universal-ai-gateway/internal/model"
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
