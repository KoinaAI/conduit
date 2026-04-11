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

func TestPrepareUpstreamExchangeGeminiStreamForcesSSEQuery(t *testing.T) {
	t.Parallel()

	exchange, err := prepareUpstreamExchange(model.ProtocolGeminiStream, model.ProviderKindGemini, "/v1beta/models/gemini-2.5:streamGenerateContent", parsedProxyRequest{
		rawBody: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
	}, "gem-up")
	if err != nil {
		t.Fatalf("prepare upstream exchange: %v", err)
	}
	if exchange.QuerySet.Get("alt") != "sse" {
		t.Fatalf("expected gemini stream exchange to force alt=sse, got %+v", exchange.QuerySet)
	}
}

func TestAnthropicToOpenAIChatPreservesToolAndImageContext(t *testing.T) {
	t.Parallel()

	body, stream, err := anthropicToOpenAIChat(map[string]any{
		"stream":         true,
		"system":         "You are a router.",
		"stop_sequences": []any{"DONE"},
		"tool_choice":    map[string]any{"type": "tool", "name": "search"},
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
	if payload["stop"] != "DONE" {
		t.Fatalf("expected anthropic stop_sequences to map to openai stop, got %+v", payload["stop"])
	}
	toolChoice := payload["tool_choice"].(map[string]any)
	if toolChoice["type"] != "function" || toolChoice["function"].(map[string]any)["name"] != "search" {
		t.Fatalf("expected anthropic tool_choice to map to openai function selector, got %+v", payload["tool_choice"])
	}
}

func TestGeminiToOpenAIChatPreservesFunctionCallsAndResponses(t *testing.T) {
	t.Parallel()

	body, err := geminiToOpenAIChat([]byte(`{
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["search"]}},
		"generationConfig":{
			"stopSequences":["DONE"],
			"responseMimeType":"application/json",
			"responseJsonSchema":{"type":"object","properties":{"result":{"type":"string"}}}
		},
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
	if payload["stop"] != "DONE" {
		t.Fatalf("expected gemini stopSequences to map to openai stop, got %+v", payload["stop"])
	}
	toolChoice := payload["tool_choice"].(map[string]any)
	if toolChoice["type"] != "function" || toolChoice["function"].(map[string]any)["name"] != "search" {
		t.Fatalf("expected gemini functionCallingConfig to map to openai function selector, got %+v", payload["tool_choice"])
	}
	responseFormat := payload["response_format"].(map[string]any)
	if responseFormat["type"] != "json_schema" {
		t.Fatalf("expected gemini response JSON schema to map to openai response_format, got %+v", payload["response_format"])
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

func TestResponseInputItemToolRoleAcceptsCallIDAlias(t *testing.T) {
	t.Parallel()

	messages, err := responseInputItemToOpenAIMessages(map[string]any{
		"role":    "tool",
		"call_id": "call-1",
		"content": "sunny",
	})
	if err != nil {
		t.Fatalf("response input item to openai messages: %v", err)
	}
	if len(messages) != 1 || messages[0]["tool_call_id"] != "call-1" {
		t.Fatalf("expected tool role call_id alias to populate tool_call_id, got %+v", messages)
	}
}

func TestOpenAIChatToolMessageToGeminiUsesMessageNameWhenTrackerMisses(t *testing.T) {
	t.Parallel()

	content := openAIChatToolMessageToGemini(map[string]any{
		"role":         "tool",
		"tool_call_id": "call-1",
		"name":         "search",
		"content":      map[string]any{"result": "sunny"},
	}, map[string]string{})
	if content == nil {
		t.Fatal("expected tool message to convert to gemini function response")
	}
	functionResponse := content["parts"].([]map[string]any)[0]["functionResponse"].(map[string]any)
	if functionResponse["name"] != "search" {
		t.Fatalf("expected message name to survive gemini tool response conversion, got %+v", functionResponse)
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

func TestWriteGeminiSSEReturnsErrorOnEmptyStream(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("")),
	}
	err := writeGeminiSSE(recorder, resp, NewUsageObserver(model.ProtocolGeminiStream), "gemini-2.5", nil, resolvedCandidate{})
	if err == nil || !responseWriteStarted(err) {
		t.Fatalf("expected empty gemini SSE stream to fail after write start, got %v", err)
	}
	if strings.Contains(recorder.Body.String(), "usageMetadata") {
		t.Fatalf("did not expect empty upstream stream to fabricate gemini events, got %q", recorder.Body.String())
	}
}

func TestWriteResponsesSSEFromGeminiStreamReturnsErrorOnEmptyStream(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("")),
	}
	err := writeResponsesSSE(recorder, resp, NewUsageObserver(model.ProtocolOpenAIResponses), "gpt-5.4", nil, resolvedCandidate{
		provider: model.Provider{Kind: model.ProviderKindGemini},
	})
	if err == nil || !responseWriteStarted(err) {
		t.Fatalf("expected empty gemini responses stream to fail after write start, got %v", err)
	}
	if strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("did not expect empty upstream stream to fabricate responses events, got %q", recorder.Body.String())
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
	src.Set("Accept-Encoding", "gzip")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Proxy-Authorization", "Basic abc123")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("TE", "trailers")
	src.Set("Trailer", "X-Test")
	src.Set("X-Connection-Token", "secret")
	src.Set("X-Trace-ID", "trace-1")

	dst := http.Header{}
	copyForwardHeaders(dst, src)

	if dst.Get("Accept-Encoding") != "" || dst.Get("Proxy-Authorization") != "" || dst.Get("Transfer-Encoding") != "" || dst.Get("Keep-Alive") != "" || dst.Get("X-Connection-Token") != "" {
		t.Fatalf("expected hop-by-hop and proxy headers to be stripped, got %+v", dst)
	}
	if dst.Get("X-Trace-ID") != "trace-1" {
		t.Fatalf("expected end-to-end headers to be forwarded, got %+v", dst)
	}
}

func TestCopyResponseHeadersDropsHopByHopHeaders(t *testing.T) {
	t.Parallel()

	src := http.Header{}
	src.Set("Connection", "keep-alive, X-Upstream-Hop")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Proxy-Authenticate", "Basic realm=test")
	src.Set("Proxy-Authorization", "Basic abc123")
	src.Set("Proxy-Connection", "keep-alive")
	src.Set("TE", "trailers")
	src.Set("Trailer", "X-Test")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Upgrade", "websocket")
	src.Set("Sec-WebSocket-Accept", "token")
	src.Set("X-Upstream-Hop", "secret")
	src.Set("X-Upstream-Trace", "trace-1")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	for _, key := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"Sec-WebSocket-Accept",
		"X-Upstream-Hop",
	} {
		if value := dst.Get(key); value != "" {
			t.Fatalf("expected response hop-by-hop header %s to be stripped, got %+v", key, dst)
		}
	}
	if dst.Get("X-Upstream-Trace") != "trace-1" {
		t.Fatalf("expected end-to-end response header to be preserved, got %+v", dst)
	}
}

func TestOpenAIChatToolChoiceToGeminiAcceptsCanonicalToolSelector(t *testing.T) {
	t.Parallel()

	config := openAIChatToolChoiceToGemini(map[string]any{
		"type": "tool",
		"name": "search",
	})
	if config == nil {
		t.Fatal("expected canonical tool selector to be preserved for gemini")
	}
	functionCallingConfig := config["functionCallingConfig"].(map[string]any)
	if functionCallingConfig["mode"] != "ANY" {
		t.Fatalf("expected canonical tool selector to map to ANY mode, got %+v", config)
	}
	allowed := functionCallingConfig["allowedFunctionNames"].([]string)
	if len(allowed) != 1 || allowed[0] != "search" {
		t.Fatalf("expected canonical tool selector to preserve allowed function name, got %+v", config)
	}
}

func TestOpenAIChatToolChoiceToGeminiAcceptsCanonicalAnySelector(t *testing.T) {
	t.Parallel()

	config := openAIChatToolChoiceToGemini("any")
	if config == nil {
		t.Fatal("expected canonical any selector to be preserved for gemini")
	}
	functionCallingConfig := config["functionCallingConfig"].(map[string]any)
	if functionCallingConfig["mode"] != "ANY" {
		t.Fatalf("expected canonical any selector to map to gemini ANY mode, got %+v", config)
	}
}

func TestOpenAIChatToolChoiceToAnthropicAcceptsCanonicalAnySelector(t *testing.T) {
	t.Parallel()

	config := openAIChatToolChoiceToAnthropic("any")
	if config == nil {
		t.Fatal("expected canonical any selector to be preserved for anthropic")
	}
	if config["type"] != "any" {
		t.Fatalf("expected canonical any selector to map to anthropic any type, got %+v", config)
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

func TestBuildCandidatePlanRejectsCoolingCredentials(t *testing.T) {
	t.Parallel()

	fileStore, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state := model.DefaultState()
	state.PricingProfiles = []model.PricingProfile{{ID: "standard", Name: "standard", Currency: "USD"}}
	state.Providers = []model.Provider{{
		ID:           "provider-1",
		Name:         "Provider 1",
		Kind:         model.ProviderKindOpenAICompatible,
		Enabled:      true,
		Capabilities: []model.Protocol{model.ProtocolOpenAIChat},
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
		Alias: "gpt-5.4",
		Targets: []model.RouteTarget{{
			ID:               "target-1",
			AccountID:        "provider-1",
			UpstreamModel:    "upstream-gpt-5.4",
			Weight:           1,
			Enabled:          true,
			MarkupMultiplier: 1,
		}},
		PricingProfileID: "standard",
	}}
	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	service := NewService(config.Config{}, fileStore)
	candidate := resolvedCandidate{
		provider:   state.Providers[0],
		endpoint:   state.Providers[0].Endpoints[0],
		credential: state.Providers[0].Credentials[0],
	}
	service.runtime.reportFailure(candidate, http.StatusTooManyRequests, "slow down", 5*time.Minute, false)

	_, _, _, _, err = service.buildCandidatePlan(fileStore.RoutingSnapshot(), "gpt-5.4", model.ProtocolOpenAIChat, "gk-1", "session-1", "")
	if err == nil || !strings.Contains(err.Error(), "no healthy provider") {
		t.Fatalf("expected cooled-down credential to be excluded from routing, got %v", err)
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
			"tool_choice": map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "search",
				},
			},
			"stop": "DONE",
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
	toolChoice := payload["tool_choice"].(map[string]any)
	if toolChoice["type"] != "tool" || toolChoice["name"] != "search" {
		t.Fatalf("expected responses tool_choice to become anthropic tool selector, got %+v", payload["tool_choice"])
	}
	stopSequences := payload["stop_sequences"].([]any)
	if len(stopSequences) != 1 || stopSequences[0] != "DONE" {
		t.Fatalf("expected responses stop to become anthropic stop_sequences, got %+v", payload["stop_sequences"])
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
			"tool_choice": "required",
			"stop":        "DONE",
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
	toolChoice := payload["tool_choice"].(map[string]any)
	if toolChoice["type"] != "any" {
		t.Fatalf("expected required tool_choice to become anthropic any selector, got %+v", payload["tool_choice"])
	}
	stopSequences := payload["stop_sequences"].([]any)
	if len(stopSequences) != 1 || stopSequences[0] != "DONE" {
		t.Fatalf("expected chat stop to become anthropic stop_sequences, got %+v", payload["stop_sequences"])
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
			"text": map[string]any{
				"format": map[string]any{
					"type": "json_object",
				},
			},
			"stop": []any{"DONE", "HALT"},
			"tool_choice": map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "search",
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
	generationConfig := payload["generationConfig"].(map[string]any)
	if generationConfig["responseMimeType"] != "application/json" {
		t.Fatalf("expected responses text.format to become gemini JSON response mime type, got %+v", generationConfig)
	}
	stopSequences := generationConfig["stopSequences"].([]any)
	if len(stopSequences) != 2 || stopSequences[0] != "DONE" || stopSequences[1] != "HALT" {
		t.Fatalf("expected responses stop to become gemini stopSequences, got %+v", generationConfig)
	}
	toolConfig := payload["toolConfig"].(map[string]any)
	functionCallingConfig := toolConfig["functionCallingConfig"].(map[string]any)
	if functionCallingConfig["mode"] != "ANY" {
		t.Fatalf("expected responses tool_choice to become gemini ANY mode, got %+v", payload["toolConfig"])
	}
	allowed := functionCallingConfig["allowedFunctionNames"].([]any)
	if len(allowed) != 1 || allowed[0] != "search" {
		t.Fatalf("expected responses tool_choice to preserve target function, got %+v", payload["toolConfig"])
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
			"response_format": map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name": "weather",
					"schema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"forecast": map[string]any{"type": "string"},
						},
						"required": []any{"forecast"},
					},
				},
			},
			"tool_choice": "required",
			"stop":        []any{"DONE", "HALT"},
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
	if exchange.QuerySet.Get("alt") != "sse" {
		t.Fatalf("expected gemini chat stream exchange to force alt=sse, got %+v", exchange.QuerySet)
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
	generationConfig := payload["generationConfig"].(map[string]any)
	if generationConfig["responseMimeType"] != "application/json" {
		t.Fatalf("expected chat response_format to become gemini JSON response mime type, got %+v", generationConfig)
	}
	stopSequences := generationConfig["stopSequences"].([]any)
	if len(stopSequences) != 2 || stopSequences[0] != "DONE" || stopSequences[1] != "HALT" {
		t.Fatalf("expected chat stop to become gemini stopSequences, got %+v", generationConfig)
	}
	responseSchema := generationConfig["responseJsonSchema"].(map[string]any)
	if responseSchema["type"] != "object" {
		t.Fatalf("expected chat response_format schema to be preserved, got %+v", generationConfig)
	}
	contents := payload["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("expected user chat message to remain in contents, got %+v", payload["contents"])
	}
	toolConfig := payload["toolConfig"].(map[string]any)
	functionCallingConfig := toolConfig["functionCallingConfig"].(map[string]any)
	if functionCallingConfig["mode"] != "ANY" {
		t.Fatalf("expected required tool_choice to become gemini ANY mode, got %+v", payload["toolConfig"])
	}
}

func TestOpenAIChatContentToGeminiPartsSkipsRemoteImageURLs(t *testing.T) {
	t.Parallel()

	parts := openAIChatContentToGeminiParts([]any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "https://example.com/cat.png",
			},
		},
	})
	if len(parts) != 1 {
		t.Fatalf("expected remote image URL to be skipped for gemini inlineData conversion, got %+v", parts)
	}
	if parts[0]["text"] != "look" {
		t.Fatalf("expected text part to remain, got %+v", parts)
	}
}

func TestOpenAIChatContentToGeminiPartsPreservesDataURLImages(t *testing.T) {
	t.Parallel()

	parts := openAIChatContentToGeminiParts([]any{
		map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:image/png;base64,ZmFrZQ==",
			},
		},
	})
	if len(parts) != 1 {
		t.Fatalf("expected one gemini inlineData image part, got %+v", parts)
	}
	inlineData, ok := parts[0]["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("expected inlineData image part, got %+v", parts[0])
	}
	if inlineData["mimeType"] != "image/png" || inlineData["data"] != "ZmFrZQ==" {
		t.Fatalf("unexpected inlineData payload: %+v", inlineData)
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
	if strings.TrimSpace(stringValue(body["id"])) == strings.TrimSpace(stringValue(output[0].(map[string]any)["id"])) {
		t.Fatalf("expected response id and output item id to be distinct, got body=%+v", body)
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

	responseID, deltaItemID, completedItemID, createdAt, completedAt := parseResponsesSSEMetadata(t, body)
	if deltaItemID == "" || completedItemID == "" {
		t.Fatalf("expected both delta and completed item ids, got delta=%q completed=%q body=%q", deltaItemID, completedItemID, body)
	}
	if deltaItemID != completedItemID {
		t.Fatalf("expected response.completed output item id to match delta item id, got delta=%q completed=%q", deltaItemID, completedItemID)
	}
	if responseID == "" {
		t.Fatalf("expected response id in response.created, got body=%q", body)
	}
	if responseID == completedItemID {
		t.Fatalf("expected response id and output item id to remain distinct, got response=%q item=%q", responseID, completedItemID)
	}
	if createdAt == 0 || completedAt == 0 {
		t.Fatalf("expected created_at in both response.created and response.completed, got created=%d completed=%d body=%q", createdAt, completedAt, body)
	}
	if createdAt != completedAt {
		t.Fatalf("expected response.created and response.completed to share created_at, got created=%d completed=%d", createdAt, completedAt)
	}
}

func TestWriteResponsesSSEFromAnthropicStreamReturnsErrorOnEmptyStream(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	resp := &http.Response{
		Body: io.NopCloser(strings.NewReader("")),
	}
	err := writeResponsesSSE(recorder, resp, NewUsageObserver(model.ProtocolOpenAIResponses), "gpt-5.4", nil, resolvedCandidate{
		provider: model.Provider{Kind: model.ProviderKindAnthropic},
	})
	if err == nil || !responseWriteStarted(err) {
		t.Fatalf("expected empty anthropic responses stream to fail after write start, got %v", err)
	}
	if strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("did not expect empty upstream stream to fabricate responses events, got %q", recorder.Body.String())
	}
}

func parseResponsesSSEMetadata(t *testing.T, body string) (string, string, string, int64, int64) {
	t.Helper()

	lines := strings.Split(body, "\n")
	currentEvent := ""
	responseID := ""
	deltaItemID := ""
	completedItemID := ""
	createdAt := int64(0)
	completedAt := int64(0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("decode sse payload for event %q: %v", currentEvent, err)
		}
		switch currentEvent {
		case "response.created":
			response, _ := decoded["response"].(map[string]any)
			responseID = strings.TrimSpace(stringValue(response["id"]))
			createdAt = int64(numberValue(response["created_at"]))
		case "response.output_text.delta":
			deltaItemID = strings.TrimSpace(stringValue(decoded["item_id"]))
		case "response.completed":
			response, _ := decoded["response"].(map[string]any)
			completedAt = int64(numberValue(response["created_at"]))
			output, _ := response["output"].([]any)
			if len(output) > 0 {
				item, _ := output[0].(map[string]any)
				completedItemID = strings.TrimSpace(stringValue(item["id"]))
			}
		}
	}
	return responseID, deltaItemID, completedItemID, createdAt, completedAt
}

func numberValue(value any) float64 {
	switch current := value.(type) {
	case float64:
		return current
	case float32:
		return float64(current)
	case int:
		return float64(current)
	case int64:
		return float64(current)
	default:
		return 0
	}
}
