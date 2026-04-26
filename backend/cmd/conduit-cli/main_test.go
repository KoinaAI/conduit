package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPrintEnv(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"print-env", "--base-url", "https://api.example", "--api-key", "gateway-secret"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected success, got code %d stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, `export OPENAI_BASE_URL="https://api.example/v1"`) {
		t.Fatalf("expected OPENAI_BASE_URL export, got %q", output)
	}
	if !strings.Contains(output, `export ANTHROPIC_API_KEY="gateway-secret"`) {
		t.Fatalf("expected ANTHROPIC_API_KEY export, got %q", output)
	}
}

func TestRunHealth(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"health", "--base-url", server.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected success, got code %d stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != `{"status":"ok"}` {
		t.Fatalf("unexpected health output: %q", stdout.String())
	}
}

func TestRunCreateKeyRequiresAdminToken(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"create-key", "--name", "laptop"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected usage error, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--admin-token") || !strings.Contains(stderr.String(), "--name are required") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestRunCreateKeyAcceptsAdminTokenFromEnv(t *testing.T) {
	t.Setenv("GATEWAY_ADMIN_TOKEN", "env-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// Missing --base-url so the request will fail, but only after the env-token
	// is accepted. We just want to confirm the env-token unblocks the required-flag check.
	code := run([]string{"create-key", "--base-url", "http://127.0.0.1:1", "--name", "laptop"}, &stdout, &stderr)
	if code == 2 {
		t.Fatalf("expected env token to satisfy admin-token requirement, got usage error: %s", stderr.String())
	}
}
