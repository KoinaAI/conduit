package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestParseAndRewriteJSONProxyRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))

	parsed, err := parseProxyRequest(model.ProtocolOpenAIResponses, req, body)
	if err != nil {
		t.Fatalf("parse proxy request: %v", err)
	}
	if parsed.routeAlias != "gpt-5.4" {
		t.Fatalf("unexpected route alias: %s", parsed.routeAlias)
	}
	if !parsed.stream {
		t.Fatalf("expected stream flag to be detected")
	}

	nextBody, path, err := rewriteProxyRequest(model.ProtocolOpenAIResponses, req, parsed, "provider-model")
	if err != nil {
		t.Fatalf("rewrite proxy request: %v", err)
	}
	if path != "/v1/responses" {
		t.Fatalf("unexpected rewritten path: %s", path)
	}
	if !strings.Contains(string(nextBody), `"model":"provider-model"`) {
		t.Fatalf("expected upstream model rewrite, got %s", string(nextBody))
	}
	if strings.Contains(string(nextBody), `"model":"gpt-5.4"`) {
		t.Fatalf("expected alias to be replaced, got %s", string(nextBody))
	}
}

func TestRewriteProxyRequestGeminiPreservesBody(t *testing.T) {
	t.Parallel()

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5:streamGenerateContent", strings.NewReader(string(body)))

	nextBody, path, err := rewriteProxyRequest(model.ProtocolGeminiStream, req, parsedProxyRequest{
		rawBody: body,
	}, "gem-up")
	if err != nil {
		t.Fatalf("rewrite gemini request: %v", err)
	}
	if path != "/v1beta/models/gem-up:streamGenerateContent" {
		t.Fatalf("unexpected rewritten gemini path: %s", path)
	}
	if string(nextBody) != string(body) {
		t.Fatalf("expected gemini body to remain unchanged, got %s", string(nextBody))
	}
}
