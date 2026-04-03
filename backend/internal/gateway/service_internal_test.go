package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/store"
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

func TestBearerTokenRejectsNonBearerSchemes(t *testing.T) {
	t.Parallel()

	if got := bearerToken("Basic abc123"); got != "" {
		t.Fatalf("expected non-bearer scheme to be ignored, got %q", got)
	}
	if got := bearerToken("bearer secret-token"); got != "secret-token" {
		t.Fatalf("expected bearer token to be parsed case-insensitively, got %q", got)
	}
}

func TestBuildCandidatePlanRespectsPerProviderMaxAttempts(t *testing.T) {
	t.Parallel()

	state := model.DefaultState()
	state.Providers = []model.Provider{
		{
			ID:             "provider-a",
			Name:           "Provider A",
			Kind:           model.ProviderKindOpenAICompatible,
			BaseURL:        "https://provider-a.example/v1",
			APIKey:         "key-a",
			Enabled:        true,
			MaxAttempts:    1,
			Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
			RoutingMode:    model.ProviderRoutingModeLatency,
			TimeoutSeconds: 30,
		},
		{
			ID:             "provider-b",
			Name:           "Provider B",
			Kind:           model.ProviderKindOpenAICompatible,
			BaseURL:        "https://provider-b.example/v1",
			APIKey:         "key-b",
			Enabled:        true,
			MaxAttempts:    1,
			Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
			RoutingMode:    model.ProviderRoutingModeLatency,
			TimeoutSeconds: 30,
		},
	}
	state.ModelRoutes = []model.ModelRoute{{
		Alias: "gpt-5.4",
		Targets: []model.RouteTarget{
			{ID: "target-a", AccountID: "provider-a", Weight: 1, Enabled: true, MarkupMultiplier: 1},
			{ID: "target-b", AccountID: "provider-b", Weight: 1, Enabled: true, MarkupMultiplier: 1},
		},
	}}
	state.Normalize()
	state.Providers[0].Credentials = append(state.Providers[0].Credentials, model.ProviderCredential{
		ID:      "cred-a-2",
		APIKey:  "key-a-2",
		Enabled: true,
		Weight:  1,
		Headers: map[string]string{},
	})

	service := &Service{runtime: newRuntimeState()}
	candidates, _, _, err := service.buildCandidatePlan(state.RoutingSnapshot(), "gpt-5.4", model.ProtocolOpenAIChat, "gk-1", "")
	if err != nil {
		t.Fatalf("build candidate plan: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected one candidate per provider, got %d", len(candidates))
	}
	if candidates[0].provider.ID == candidates[1].provider.ID {
		t.Fatalf("expected provider retry budgets to keep both providers in plan, got %+v", candidates)
	}
}

func TestRuntimeStickySweepRemovesExpiredEntries(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeState()
	runtime.sticky["expired"] = stickyBinding{
		ProviderID: "provider-a",
		ExpiresAt:  time.Now().UTC().Add(-time.Minute),
	}
	runtime.stickyWrites = defaultStickySweepInterval - 1

	runtime.reportSuccess(resolvedCandidate{
		provider: model.Provider{
			ID:                      "provider-a",
			StickySessionTTLSeconds: 300,
		},
		route: model.ModelRoute{Alias: "gpt-5.4"},
		endpoint: model.ProviderEndpoint{
			ID: "endpoint-a",
		},
		credential: model.ProviderCredential{
			ID:     "cred-a",
			APIKey: "key-a",
		},
		gatewayKeyID: "gk-1",
		sessionID:    "session-1",
	}, 10*time.Millisecond)

	if _, ok := runtime.sticky["expired"]; ok {
		t.Fatalf("expected expired sticky entry to be swept")
	}
}

func TestRunProbesDoesNotMutateLiveRuntimeState(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Fatalf("unexpected probe path: %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		service := newProbeTestService(t, model.ProviderKindOpenAICompatible, server.URL)
		result := service.RunProbes(context.Background())
		if len(result["results"].([]map[string]any)) != 1 {
			t.Fatalf("expected one probe result, got %+v", result)
		}

		service.runtime.mu.Lock()
		defer service.runtime.mu.Unlock()
		if len(service.runtime.endpoints) != 0 {
			t.Fatalf("expected probe success to avoid live endpoint mutations, got %+v", service.runtime.endpoints)
		}
		if len(service.runtime.credentials) != 0 {
			t.Fatalf("expected probe success to avoid live credential mutations, got %+v", service.runtime.credentials)
		}
		if len(service.runtime.sticky) != 0 {
			t.Fatalf("expected probe success to avoid sticky mutations, got %+v", service.runtime.sticky)
		}
	})

	t.Run("auth failure", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer server.Close()

		service := newProbeTestService(t, model.ProviderKindOpenAICompatible, server.URL)
		result := service.RunProbes(context.Background())
		if len(result["results"].([]map[string]any)) != 1 {
			t.Fatalf("expected one probe result, got %+v", result)
		}

		service.runtime.mu.Lock()
		defer service.runtime.mu.Unlock()
		if len(service.runtime.credentials) != 0 {
			t.Fatalf("expected probe auth failure to avoid disabling live credentials, got %+v", service.runtime.credentials)
		}
		if len(service.runtime.endpoints) != 0 {
			t.Fatalf("expected probe auth failure to avoid tripping live endpoint state, got %+v", service.runtime.endpoints)
		}
	})
}

func newProbeTestService(t *testing.T, kind model.ProviderKind, baseURL string) *Service {
	t.Helper()

	fileStore, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state := model.DefaultState()
	state.Providers = []model.Provider{{
		ID:      "provider-1",
		Name:    "Provider 1",
		Kind:    kind,
		Enabled: true,
		Endpoints: []model.ProviderEndpoint{{
			ID:      "endpoint-1",
			BaseURL: baseURL,
			Enabled: true,
		}},
		Credentials: []model.ProviderCredential{{
			ID:      "credential-1",
			APIKey:  "provider-key",
			Enabled: true,
		}},
	}}
	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	return NewService(config.Config{}, fileStore)
}

func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set("Retry-After", "999999999")

	if got := parseRetryAfter(headers); got != maxRetryAfterCooldown {
		t.Fatalf("expected retry-after to clamp to %v, got %v", maxRetryAfterCooldown, got)
	}
}

func TestRuntimeReportFailureClampsHugeRetryAfter(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeState()
	candidate := resolvedCandidate{
		provider:   model.Provider{ID: "provider-1"},
		endpoint:   model.ProviderEndpoint{ID: "endpoint-1"},
		credential: model.ProviderCredential{ID: "credential-1", APIKey: "credential-key"},
	}

	before := time.Now().UTC()
	runtime.reportFailure(candidate, http.StatusTooManyRequests, "ratelimited", 24*365*time.Hour)

	runtime.mu.Lock()
	credential := runtime.credentials[runtime.credentialRuntimeKeyLocked(candidate)]
	runtime.mu.Unlock()
	if credential == nil {
		t.Fatal("expected credential runtime state to be created")
	}
	if credential.DisabledUntil.Sub(before) > maxRetryAfterCooldown+time.Second {
		t.Fatalf("expected retry-after clamp within %v, got %v", maxRetryAfterCooldown, credential.DisabledUntil.Sub(before))
	}
}

func TestGeminiToOpenAIChatDoesNotDuplicateSystemInstruction(t *testing.T) {
	t.Parallel()

	body, err := geminiToOpenAIChat([]byte(`{
		"systemInstruction":{"parts":[{"text":"camel"}]},
		"system_instruction":{"parts":[{"text":"snake"}]},
		"contents":[{"role":"user","parts":[{"text":"hello"}]}]
	}`), "gpt-5.4", false)
	if err != nil {
		t.Fatalf("gemini to openai chat: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	messages := payload["messages"].([]any)
	systemCount := 0
	for _, item := range messages {
		message := item.(map[string]any)
		if message["role"] == "system" {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("expected exactly one system message, got %d body=%s", systemCount, string(body))
	}
}

func TestReadLimitedResponseBodyRejectsOversizedPayload(t *testing.T) {
	t.Parallel()

	body := bytes.NewReader(bytes.Repeat([]byte("a"), maxTransformedResponseBodyBytes+1))
	if _, err := readLimitedResponseBody(body, maxTransformedResponseBodyBytes); err == nil {
		t.Fatal("expected oversized upstream response to be rejected")
	}
}
