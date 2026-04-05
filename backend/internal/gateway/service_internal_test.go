package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestAnthropicToOpenAIChatPreservesToolAndImageContext(t *testing.T) {
	t.Parallel()

	body, stream, err := anthropicToOpenAIChat(map[string]any{
		"stream": true,
		"system": "You are a router.",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "ZmFrZQ==",
						},
					},
					map[string]any{"type": "text", "text": "look at this"},
				},
			},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "calling tool"},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "search", "input": map[string]any{"q": "weather"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{map[string]any{"type": "text", "text": "sunny"}}},
				},
			},
		},
		"tools": []any{
			map[string]any{"name": "search", "description": "search the web", "input_schema": map[string]any{"type": "object"}},
		},
	}, "provider-model")
	if err != nil {
		t.Fatalf("anthropic to openai chat: %v", err)
	}
	if !stream {
		t.Fatal("expected stream flag to be preserved")
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected system + user + assistant + tool messages, got %d", len(messages))
	}
	user := messages[1].(map[string]any)
	content := user["content"].([]any)
	if content[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("expected anthropic image block to become image_url content, got %+v", content[0])
	}
	assistant := messages[2].(map[string]any)
	toolCalls := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected one tool call, got %+v", assistant)
	}
	toolMessage := messages[3].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != "toolu_1" || toolMessage["content"] != "sunny" {
		t.Fatalf("expected tool result message to be preserved, got %+v", toolMessage)
	}
}

func TestGeminiToOpenAIChatPreservesFunctionCallsAndResponses(t *testing.T) {
	t.Parallel()

	body, err := geminiToOpenAIChat([]byte(`{
		"contents":[
			{"role":"user","parts":[{"text":"weather?"}]},
			{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"weather"}}}]},
			{"role":"function","parts":[{"functionResponse":{"name":"search","response":{"result":"sunny"}}}]}
		]
	}`), "provider-model", false)
	if err != nil {
		t.Fatalf("gemini to openai chat: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected user + assistant + tool messages, got %d", len(messages))
	}
	assistant := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("expected function call to map to assistant message, got %+v", assistant)
	}
	toolCalls := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("expected one tool call, got %+v", assistant)
	}
	callID := toolCalls[0].(map[string]any)["id"]
	toolMessage := messages[2].(map[string]any)
	if toolMessage["role"] != "tool" || toolMessage["tool_call_id"] != callID {
		t.Fatalf("expected function response to map to tool message, got %+v", toolMessage)
	}
	if !strings.Contains(toolMessage["content"].(string), `"result":"sunny"`) {
		t.Fatalf("expected function response payload to be preserved, got %+v", toolMessage)
	}
}

func TestGeminiToOpenAIChatSkipsFunctionResponseWithoutName(t *testing.T) {
	t.Parallel()

	body, err := geminiToOpenAIChat([]byte(`{
		"contents":[
			{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"weather"}}}]},
			{"role":"function","parts":[{"functionResponse":{"response":{"result":"sunny"}}}]}
		]
	}`), "provider-model", false)
	if err != nil {
		t.Fatalf("gemini to openai chat: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected nameless function response to be skipped, got %d messages", len(messages))
	}
	assistant := messages[0].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("expected remaining message to be assistant tool call, got %+v", assistant)
	}
}

func TestWriteAnthropicSSEReturnsErrorOnMalformedJSONEvent(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("data: {bad json}\n\n")),
	}
	err := writeAnthropicSSE(recorder, resp, NewUsageObserver(model.ProtocolOpenAIChat), "gpt-5.4", nil, resolvedCandidate{})
	if err == nil || !responseWriteStarted(err) {
		t.Fatalf("expected malformed SSE payload to fail after write start, got %v", err)
	}
}

func TestWriteAnthropicSSEReturnsErrorOnEmptyStream(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("")),
	}
	err := writeAnthropicSSE(recorder, resp, NewUsageObserver(model.ProtocolOpenAIChat), "gpt-5.4", nil, resolvedCandidate{})
	if err == nil || !responseWriteStarted(err) {
		t.Fatalf("expected empty SSE stream to fail after write start, got %v", err)
	}
	if strings.Contains(recorder.Body.String(), "message_start") {
		t.Fatalf("did not expect empty upstream stream to fabricate anthropic events, got %q", recorder.Body.String())
	}
}

func TestMapAnthropicStopReasonContentFilterBecomesRefusal(t *testing.T) {
	t.Parallel()

	if got := mapAnthropicStopReason("content_filter"); got != "refusal" {
		t.Fatalf("expected content_filter to map to refusal, got %q", got)
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

func TestReadTokenIntFallsBackPastZeroValue(t *testing.T) {
	t.Parallel()

	if got := readTokenInt(map[string]any{
		"prompt_tokens": float64(0),
		"input_tokens":  float64(500),
	}, "prompt_tokens", "input_tokens", "promptTokenCount"); got != 500 {
		t.Fatalf("expected later token keys to be considered when earlier alias is zero, got %d", got)
	}
}

func TestCopyForwardHeadersDropsHopByHopAndProxyCredentials(t *testing.T) {
	t.Parallel()

	src := http.Header{}
	src.Set("Connection", "Keep-Alive, X-Connection-Token")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Proxy-Authorization", "Basic abc123")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("TE", "trailers")
	src.Set("Trailer", "X-Test")
	src.Set("X-Connection-Token", "secret")
	src.Set("X-Trace-ID", "trace-1")

	dst := http.Header{}
	copyForwardHeaders(dst, src)

	if dst.Get("Proxy-Authorization") != "" || dst.Get("Transfer-Encoding") != "" || dst.Get("Keep-Alive") != "" || dst.Get("X-Connection-Token") != "" {
		t.Fatalf("expected hop-by-hop and proxy headers to be stripped, got %+v", dst)
	}
	if dst.Get("X-Trace-ID") != "trace-1" {
		t.Fatalf("expected end-to-end headers to be forwarded, got %+v", dst)
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
			Enabled:        true,
			MaxAttempts:    1,
			Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
			RoutingMode:    model.ProviderRoutingModeLatency,
			TimeoutSeconds: 30,
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-a",
				BaseURL: "https://provider-a.example/v1",
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{
				ID:      "cred-a-1",
				APIKey:  "key-a",
				Enabled: true,
			}},
		},
		{
			ID:             "provider-b",
			Name:           "Provider B",
			Kind:           model.ProviderKindOpenAICompatible,
			Enabled:        true,
			MaxAttempts:    1,
			Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
			RoutingMode:    model.ProviderRoutingModeLatency,
			TimeoutSeconds: 30,
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-b",
				BaseURL: "https://provider-b.example/v1",
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{
				ID:      "cred-b-1",
				APIKey:  "key-b",
				Enabled: true,
			}},
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
	candidates, _, _, _, err := service.buildCandidatePlan(state.RoutingSnapshot(), "gpt-5.4", model.ProtocolOpenAIChat, "gk-1", "", "")
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
	}, 10*time.Millisecond, false)

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
	runtime.reportFailure(candidate, http.StatusTooManyRequests, "ratelimited", 24*365*time.Hour, false)

	runtime.mu.Lock()
	credential := runtime.credentials[credentialRuntimeKey(candidate)]
	runtime.mu.Unlock()
	if credential == nil {
		t.Fatal("expected credential runtime state to be created")
	}
	if credential.DisabledUntil.Sub(before) > maxRetryAfterCooldown+time.Second {
		t.Fatalf("expected retry-after clamp within %v, got %v", maxRetryAfterCooldown, credential.DisabledUntil.Sub(before))
	}
}

func TestBuildCandidatePlanResolvesPricingAliasRules(t *testing.T) {
	t.Parallel()

	fileStore, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state := model.DefaultState()
	state.Providers = []model.Provider{{
		ID:                      "provider-1",
		Name:                    "Provider 1",
		Kind:                    model.ProviderKindOpenAICompatible,
		Enabled:                 true,
		DefaultMarkupMultiplier: 1,
		Capabilities:            []model.Protocol{model.ProtocolOpenAIChat},
		Endpoints: []model.ProviderEndpoint{{
			ID:      "endpoint-1",
			BaseURL: "https://provider.example/v1",
			Enabled: true,
		}},
		Credentials: []model.ProviderCredential{{
			ID:      "credential-1",
			APIKey:  "provider-key",
			Enabled: true,
		}},
	}}
	state.ModelRoutes = []model.ModelRoute{{
		Alias: "gpt-5.4-fast",
		Targets: []model.RouteTarget{{
			ID:               "target-1",
			AccountID:        "provider-1",
			UpstreamModel:    "openai/gpt-5.4-fast",
			Weight:           1,
			Enabled:          true,
			MarkupMultiplier: 1,
		}},
	}}
	state.PricingProfiles = []model.PricingProfile{{ID: "pricing-gpt5", Name: "GPT-5"}}
	state.PricingAliases = []model.PricingAliasRule{{
		Name:             "gpt5-prefix",
		MatchType:        model.PricingAliasMatchPrefix,
		Pattern:          "gpt-5",
		PricingProfileID: "pricing-gpt5",
		Enabled:          true,
	}}
	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	service := NewService(config.Config{}, fileStore)
	candidates, _, profile, _, err := service.buildCandidatePlan(fileStore.RoutingSnapshot(), "gpt-5.4-fast", model.ProtocolOpenAIChat, "gk-1", "", "")
	if err != nil {
		t.Fatalf("build candidate plan: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected one candidate, got %+v", candidates)
	}
	if candidates[0].pricing.ID != "pricing-gpt5" || profile.ID != "pricing-gpt5" {
		t.Fatalf("expected pricing alias rule to resolve pricing-gpt5, got candidate=%+v profile=%+v", candidates[0].pricing, profile)
	}
}

func TestServiceCircuitStatusesAndReset(t *testing.T) {
	t.Parallel()

	service := newProbeTestService(t, model.ProviderKindOpenAICompatible, "https://provider.example")
	snapshot := service.store.RoutingSnapshot()
	provider := snapshot.Providers[0]
	provider.CircuitBreaker = model.CircuitBreakerConfig{
		Enabled:             testBoolPointer(true),
		FailureThreshold:    2,
		CooldownSeconds:     60,
		HalfOpenMaxRequests: 1,
	}
	candidate := resolvedCandidate{
		provider:   provider,
		endpoint:   provider.Endpoints[0],
		credential: provider.Credentials[0],
	}

	service.runtime.reportFailure(candidate, http.StatusBadGateway, "upstream failed", 0, false)
	service.runtime.reportFailure(candidate, http.StatusBadGateway, "upstream failed", 0, false)

	items := service.CircuitStatuses()
	if len(items) != 1 {
		t.Fatalf("expected one circuit row, got %+v", items)
	}
	if items[0].State != "open" || items[0].ConsecutiveFailures < 2 {
		t.Fatalf("expected open circuit status, got %+v", items[0])
	}

	if reset := service.ResetCircuits(provider.ID, provider.Endpoints[0].ID); reset != 1 {
		t.Fatalf("expected one reset endpoint, got %d", reset)
	}
	items = service.CircuitStatuses()
	if len(items) != 1 || items[0].State != "closed" || items[0].ConsecutiveFailures != 0 {
		t.Fatalf("expected reset to close the circuit, got %+v", items)
	}
}

func TestServiceStickyBindingsAndReset(t *testing.T) {
	t.Parallel()

	service := newProbeTestService(t, model.ProviderKindOpenAICompatible, "https://provider.example")
	now := time.Now().UTC()
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID:                      "provider-1",
			StickySessionTTLSeconds: 300,
		},
		route:        model.ModelRoute{Alias: "gpt-5.4"},
		scenario:     "codex",
		endpoint:     model.ProviderEndpoint{ID: "endpoint-1"},
		credential:   model.ProviderCredential{ID: "credential-1", APIKey: "provider-key"},
		gatewayKeyID: "gk-1",
	}

	service.runtime.bindStickyCandidate(candidate, "session-1", now)

	items := service.StickyBindings()
	if len(items) != 1 {
		t.Fatalf("expected one sticky binding, got %+v", items)
	}
	if items[0].GatewayKeyID != "gk-1" || items[0].RouteAlias != "gpt-5.4" || items[0].Scenario != "codex" || items[0].SessionID != "session-1" {
		t.Fatalf("unexpected sticky binding payload: %+v", items[0])
	}

	if reset := service.ResetStickyBindings("gk-1", "gpt-5.4", "codex", "session-1"); reset != 1 {
		t.Fatalf("expected one sticky binding reset, got %d", reset)
	}
	if items = service.StickyBindings(); len(items) != 0 {
		t.Fatalf("expected sticky bindings to be cleared, got %+v", items)
	}
}

func testBoolPointer(value bool) *bool {
	v := value
	return &v
}

func TestRuntimeCircuitBreakerHalfOpenLimitsProbeConcurrency(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeState()
	enabled := true
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID: "provider-1",
			CircuitBreaker: model.CircuitBreakerConfig{
				Enabled:             &enabled,
				FailureThreshold:    1,
				CooldownSeconds:     1,
				HalfOpenMaxRequests: 1,
			},
		},
		endpoint:   model.ProviderEndpoint{ID: "endpoint-1"},
		credential: model.ProviderCredential{ID: "credential-1", APIKey: "credential-key"},
	}

	runtime.reportFailure(candidate, http.StatusBadGateway, "boom", 0, false)
	if !runtime.endpointOpen(candidate, time.Now().UTC()) {
		t.Fatal("expected endpoint to open after threshold failure")
	}

	probeAt := time.Now().UTC().Add(2 * time.Second)
	if runtime.endpointOpen(candidate, probeAt) {
		t.Fatal("expected cooldown expiry to allow a half-open probe")
	}

	halfOpen, err := runtime.acquireEndpoint(candidate, probeAt)
	if err != nil {
		t.Fatalf("acquire half-open probe: %v", err)
	}
	if !halfOpen {
		t.Fatal("expected acquire to mark the probe as half-open")
	}
	if !runtime.endpointOpen(candidate, probeAt) {
		t.Fatal("expected half-open slot exhaustion to block additional probes")
	}
	if _, err := runtime.acquireEndpoint(candidate, probeAt); err != errEndpointCircuitOpen {
		t.Fatalf("expected second probe to be blocked, got %v", err)
	}

	runtime.reportSuccess(candidate, 5*time.Millisecond, halfOpen)
	if runtime.endpointOpen(candidate, probeAt) {
		t.Fatal("expected successful half-open probe to close the circuit")
	}
}

func TestRuntimeCircuitBreakerHalfOpenFailureReopensCircuit(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeState()
	enabled := true
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID: "provider-1",
			CircuitBreaker: model.CircuitBreakerConfig{
				Enabled:             &enabled,
				FailureThreshold:    1,
				CooldownSeconds:     60,
				HalfOpenMaxRequests: 1,
			},
		},
		endpoint:   model.ProviderEndpoint{ID: "endpoint-1"},
		credential: model.ProviderCredential{ID: "credential-1", APIKey: "credential-key"},
	}

	runtime.reportFailure(candidate, http.StatusBadGateway, "boom", 0, false)

	probeAt := time.Now().UTC().Add(61 * time.Second)
	halfOpen, err := runtime.acquireEndpoint(candidate, probeAt)
	if err != nil {
		t.Fatalf("acquire half-open probe: %v", err)
	}
	runtime.reportFailure(candidate, http.StatusBadGateway, "boom again", 0, halfOpen)

	if !runtime.endpointOpen(candidate, time.Now().UTC()) {
		t.Fatal("expected failed half-open probe to reopen the circuit")
	}
}

func TestGeminiToOpenAIChatDoesNotDuplicateSystemInstruction(t *testing.T) {
	t.Parallel()

	body, err := geminiToOpenAIChat([]byte(`{
		"systemInstruction":{"parts":[{"text":"camel"}]},
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

func TestPrepareUpstreamExchangeResponsesToAnthropic(t *testing.T) {
	t.Parallel()

	exchange, err := prepareUpstreamExchange(model.ProtocolOpenAIResponses, model.ProviderKindAnthropic, "/v1/responses", parsedProxyRequest{
		routeAlias: "gpt-5.4",
		jsonPayload: map[string]any{
			"stream":       true,
			"instructions": "be concise",
			"input": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": "hello"},
					},
				},
				map[string]any{
					"type":      "function_call",
					"call_id":   "call-1",
					"name":      "search",
					"arguments": map[string]any{"q": "weather"},
				},
				map[string]any{
					"type":    "function_call_output",
					"call_id": "call-1",
					"output":  map[string]any{"result": "sunny"},
				},
			},
			"tools": []any{
				map[string]any{
					"name":        "search",
					"description": "search the web",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}, "claude-3-7-sonnet")
	if err != nil {
		t.Fatalf("prepare upstream exchange: %v", err)
	}
	if exchange.Path != "/v1/messages" {
		t.Fatalf("unexpected upstream path: %s", exchange.Path)
	}
	if !exchange.Stream {
		t.Fatal("expected responses stream mode to be preserved")
	}
	if exchange.ResponseMode != responseModeResponsesSSE {
		t.Fatalf("unexpected response mode: %s", exchange.ResponseMode)
	}

	var payload map[string]any
	if err := json.Unmarshal(exchange.Body, &payload); err != nil {
		t.Fatalf("decode anthropic payload: %v", err)
	}
	if payload["model"] != "claude-3-7-sonnet" {
		t.Fatalf("expected upstream model rewrite, got %+v", payload["model"])
	}
	system := payload["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != "be concise" {
		t.Fatalf("expected instructions to become anthropic system blocks, got %+v", payload["system"])
	}
	messages := payload["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected user, assistant tool_use, user tool_result messages, got %d", len(messages))
	}
	assistant := messages[1].(map[string]any)
	content := assistant["content"].([]any)
	if content[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("expected function call to become tool_use block, got %+v", content)
	}
	toolResult := messages[2].(map[string]any)
	if toolResult["role"] != "user" {
		t.Fatalf("expected function output to become user tool_result message, got %+v", toolResult)
	}
}

func TestPrepareUpstreamExchangeChatToAnthropic(t *testing.T) {
	t.Parallel()

	exchange, err := prepareUpstreamExchange(model.ProtocolOpenAIChat, model.ProviderKindAnthropic, "/v1/chat/completions", parsedProxyRequest{
		routeAlias: "gpt-5.4",
		stream:     true,
		jsonPayload: map[string]any{
			"model":    "gpt-5.4",
			"stream":   true,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":       "search",
						"parameters": map[string]any{"type": "object"},
					},
				},
			},
		},
	}, "claude-3-7-sonnet")
	if err != nil {
		t.Fatalf("prepare upstream exchange: %v", err)
	}
	if exchange.Path != "/v1/messages" {
		t.Fatalf("unexpected upstream path: %s", exchange.Path)
	}
	if !exchange.Stream {
		t.Fatal("expected stream flag to be preserved")
	}
	if exchange.ResponseMode != responseModeAnthropicSSE {
		t.Fatalf("unexpected response mode: %s", exchange.ResponseMode)
	}

	var payload map[string]any
	if err := json.Unmarshal(exchange.Body, &payload); err != nil {
		t.Fatalf("decode anthropic payload: %v", err)
	}
	if payload["model"] != "claude-3-7-sonnet" {
		t.Fatalf("expected upstream model rewrite for anthropic conversion, got %+v", payload["model"])
	}
	messages := payload["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("expected user chat message to be preserved, got %+v", payload["messages"])
	}
	if len(payload["tools"].([]any)) != 1 {
		t.Fatalf("expected tool definitions to be preserved, got %+v", payload["tools"])
	}
}

func TestPrepareUpstreamExchangeResponsesToGemini(t *testing.T) {
	t.Parallel()

	exchange, err := prepareUpstreamExchange(model.ProtocolOpenAIResponses, model.ProviderKindGemini, "/v1/responses", parsedProxyRequest{
		routeAlias: "gpt-5.4",
		jsonPayload: map[string]any{
			"input": []any{
				map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": "hello"},
					},
				},
				map[string]any{
					"type":      "function_call",
					"call_id":   "call-1",
					"name":      "search",
					"arguments": map[string]any{"q": "weather"},
				},
				map[string]any{
					"type":    "function_call_output",
					"call_id": "call-1",
					"output":  map[string]any{"result": "sunny"},
				},
			},
			"tools": []any{
				map[string]any{
					"name":       "search",
					"parameters": map[string]any{"type": "object"},
				},
			},
		},
	}, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("prepare upstream exchange: %v", err)
	}
	if exchange.Path != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("unexpected upstream path: %s", exchange.Path)
	}
	if exchange.ResponseMode != responseModeResponsesJSON {
		t.Fatalf("unexpected response mode: %s", exchange.ResponseMode)
	}

	var payload map[string]any
	if err := json.Unmarshal(exchange.Body, &payload); err != nil {
		t.Fatalf("decode gemini payload: %v", err)
	}
	contents := payload["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("expected user, model functionCall, function response contents, got %d", len(contents))
	}
	functionMessage := contents[2].(map[string]any)
	if functionMessage["role"] != "function" {
		t.Fatalf("expected tool output to become gemini function role, got %+v", functionMessage)
	}
	tools := payload["tools"].([]any)
	declarations := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if len(declarations) != 1 {
		t.Fatalf("expected one gemini function declaration, got %+v", tools)
	}
}

func TestPrepareUpstreamExchangeChatToGemini(t *testing.T) {
	t.Parallel()

	exchange, err := prepareUpstreamExchange(model.ProtocolOpenAIChat, model.ProviderKindGemini, "/v1/chat/completions", parsedProxyRequest{
		routeAlias: "gpt-5.4",
		stream:     true,
		jsonPayload: map[string]any{
			"model":  "gpt-5.4",
			"stream": true,
			"messages": []any{
				map[string]any{"role": "system", "content": "be concise"},
				map[string]any{"role": "user", "content": "hello"},
			},
			"tools": []any{
				map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":       "search",
						"parameters": map[string]any{"type": "object"},
					},
				},
			},
		},
	}, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("prepare upstream exchange: %v", err)
	}
	if exchange.Path != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("unexpected upstream path: %s", exchange.Path)
	}
	if exchange.ResponseMode != responseModeGeminiSSE {
		t.Fatalf("unexpected response mode: %s", exchange.ResponseMode)
	}

	var payload map[string]any
	if err := json.Unmarshal(exchange.Body, &payload); err != nil {
		t.Fatalf("decode gemini payload: %v", err)
	}
	systemInstruction := payload["systemInstruction"].(map[string]any)
	systemParts := systemInstruction["parts"].([]any)
	if len(systemParts) != 1 || systemParts[0].(map[string]any)["text"] != "be concise" {
		t.Fatalf("expected system chat message to become systemInstruction, got %+v", payload["systemInstruction"])
	}
	contents := payload["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("expected user chat message to remain in contents, got %+v", payload["contents"])
	}
}

func TestWriteResponsesJSONFromAnthropicPayload(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(`{
			"id":"msg_1",
			"content":[
				{"type":"text","text":"hello"},
				{"type":"tool_use","id":"call-1","name":"search","input":{"q":"weather"}}
			]
		}`)),
	}

	err := writeResponsesJSON(recorder, resp, NewUsageObserver(model.ProtocolOpenAIResponses), "gpt-5.4", nil, resolvedCandidate{
		provider: model.Provider{Kind: model.ProviderKindAnthropic},
	})
	if err != nil {
		t.Fatalf("write responses json: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if body["object"] != "response" || body["output_text"] != "hello" {
		t.Fatalf("unexpected responses envelope: %+v", body)
	}
	output := body["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected assistant message plus tool call output, got %+v", output)
	}
	if output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("expected tool_use to map to function_call output item, got %+v", output[1])
	}
}

func TestWriteResponsesSSEFromAnthropicStream(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: message_start",
			`data: {"message":{"id":"msg_1"}}`,
			"",
			"event: content_block_delta",
			`data: {"delta":{"type":"text_delta","text":"hello"}}`,
			"",
		}, "\n"))),
	}

	err := writeResponsesSSE(recorder, resp, NewUsageObserver(model.ProtocolOpenAIResponses), "gpt-5.4", nil, resolvedCandidate{
		provider: model.Provider{Kind: model.ProviderKindAnthropic},
	})
	if err != nil {
		t.Fatalf("write responses sse: %v", err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.created") {
		t.Fatalf("expected response.created event, got %q", body)
	}
	if !strings.Contains(body, "event: response.output_text.delta") {
		t.Fatalf("expected response.output_text.delta event, got %q", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected response.completed event, got %q", body)
	}
}
