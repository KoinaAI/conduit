package gateway_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestGatewayProtocolBridgesMutually(t *testing.T) {
	openAIRequests := make(chan observedRequest, 32)
	anthropicRequests := make(chan observedRequest, 16)
	geminiRequests := make(chan observedRequest, 16)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		openAIRequests <- observedRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			Body:          string(body),
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"id":"chatcmpl_bridge","choices":[{"index":0,"message":{"role":"assistant","content":"openai-chat-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
		case "/v1/responses":
			streaming := strings.Contains(string(body), `"stream":true`)
			if streaming {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: response.created\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bridge\",\"output\":[]}}\n\n"))
				_, _ = w.Write([]byte("event: response.output_text.done\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.done\",\"text\":\"openai-response-ok\"}\n\n"))
				_, _ = w.Write([]byte("event: response.completed\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":9,\"total_tokens\":14}}}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n"))
				return
			}
			_, _ = w.Write([]byte(`{"id":"resp_bridge","object":"response","status":"completed","output":[{"id":"msg_bridge","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"openai-response-ok"}]}],"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openAIServer.Close()

	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		anthropicRequests <- observedRequest{
			Path:   r.URL.Path,
			APIKey: r.Header.Get("x-api-key"),
			Body:   string(body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_bridge","type":"message","role":"assistant","content":[{"type":"text","text":"anthropic-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`))
	}))
	defer anthropicServer.Close()

	geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		geminiRequests <- observedRequest{
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			APIKey: r.Header.Get("x-goog-api-key"),
			Body:   string(body),
		}
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"gemini-stream-ok\"}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[]}}],\"usageMetadata\":{\"promptTokenCount\":6,\"candidatesTokenCount\":8,\"totalTokenCount\":14}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"gemini-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":8,"totalTokenCount":14},"modelVersion":"gem-up"}`))
	}))
	defer geminiServer.Close()

	state := model.DefaultState()
	state.Providers = []model.Provider{
		{
			ID:                      "openai-chat",
			Name:                    "openai-chat",
			Kind:                    model.ProviderKindOpenAICompatible,
			BaseURL:                 openAIServer.URL + "/v1",
			APIKey:                  "openai-chat-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1,
			Capabilities:            []model.Protocol{model.ProtocolOpenAIChat},
		},
		{
			ID:                      "openai-responses",
			Name:                    "openai-responses",
			Kind:                    model.ProviderKindOpenAICompatible,
			BaseURL:                 openAIServer.URL + "/v1",
			APIKey:                  "openai-resp-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1,
			Capabilities:            []model.Protocol{model.ProtocolOpenAIResponses},
		},
		{
			ID:                      "anthropic",
			Name:                    "anthropic",
			Kind:                    model.ProviderKindAnthropic,
			BaseURL:                 anthropicServer.URL,
			APIKey:                  "anthropic-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1,
			Capabilities:            []model.Protocol{model.ProtocolAnthropic},
		},
		{
			ID:                      "gemini",
			Name:                    "gemini",
			Kind:                    model.ProviderKindGemini,
			BaseURL:                 geminiServer.URL,
			APIKey:                  "gemini-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1,
			Capabilities:            []model.Protocol{model.ProtocolGeminiGenerate, model.ProtocolGeminiStream},
		},
	}
	state.ModelRoutes = []model.ModelRoute{
		{Alias: "openai-chat-model", Targets: []model.RouteTarget{{ID: "t-chat", AccountID: "openai-chat", UpstreamModel: "up-openai-chat", Weight: 1, Enabled: true, MarkupMultiplier: 1}}},
		{Alias: "openai-resp-model", Targets: []model.RouteTarget{{ID: "t-resp", AccountID: "openai-responses", UpstreamModel: "up-openai-resp", Weight: 1, Enabled: true, MarkupMultiplier: 1}}},
		{Alias: "claude-bridge", Targets: []model.RouteTarget{{ID: "t-anth", AccountID: "anthropic", UpstreamModel: "claude-up", Weight: 1, Enabled: true, MarkupMultiplier: 1}}},
		{Alias: "gemini-bridge", Targets: []model.RouteTarget{{ID: "t-gem", AccountID: "gemini", UpstreamModel: "gem-up", Weight: 1, Enabled: true, MarkupMultiplier: 1}}},
	}

	gatewayServer := startGatewayServer(t, state)
	defer gatewayServer.Close()

	testCases := []struct {
		name               string
		endpoint           string
		payload            string
		headers            map[string]string
		expectBodyContains string
		expectUpstreamPath string
		expectUpstreamBody []string
		expectAuth         string
		requests           chan observedRequest
	}{
		{
			name:               "openai chat to anthropic",
			endpoint:           gatewayServer.URL + "/v1/chat/completions",
			payload:            `{"model":"claude-bridge","messages":[{"role":"user","content":"hello"}]}`,
			expectBodyContains: `"anthropic-ok"`,
			expectUpstreamPath: "/v1/messages",
			expectUpstreamBody: []string{`"messages":[{"content":[{"text":"hello","type":"text"}],"role":"user"}]`},
			expectAuth:         "anthropic-key",
			requests:           anthropicRequests,
		},
		{
			name:               "openai responses to anthropic",
			endpoint:           gatewayServer.URL + "/v1/responses",
			payload:            `{"model":"claude-bridge","input":"hello"}`,
			expectBodyContains: `anthropic-ok`,
			expectUpstreamPath: "/v1/messages",
			expectUpstreamBody: []string{`"messages":[{"content":[{"text":"hello","type":"text"}],"role":"user"}]`},
			expectAuth:         "anthropic-key",
			requests:           anthropicRequests,
		},
		{
			name:               "openai chat to gemini",
			endpoint:           gatewayServer.URL + "/v1/chat/completions",
			payload:            `{"model":"gemini-bridge","messages":[{"role":"user","content":"hello"}]}`,
			expectBodyContains: `gemini-ok`,
			expectUpstreamPath: "/v1beta/models/gem-up:generateContent",
			expectUpstreamBody: []string{`"contents":[{"parts":[{"text":"hello"}],"role":"user"}]`},
			expectAuth:         "gemini-key",
			requests:           geminiRequests,
		},
		{
			name:               "openai responses to gemini",
			endpoint:           gatewayServer.URL + "/v1/responses",
			payload:            `{"model":"gemini-bridge","input":"hello"}`,
			expectBodyContains: `gemini-ok`,
			expectUpstreamPath: "/v1beta/models/gem-up:generateContent",
			expectUpstreamBody: []string{`"contents":[{"parts":[{"text":"hello"}],"role":"user"}]`},
			expectAuth:         "gemini-key",
			requests:           geminiRequests,
		},
		{
			name:               "openai chat to openai responses upstream",
			endpoint:           gatewayServer.URL + "/v1/chat/completions",
			payload:            `{"model":"openai-resp-model","messages":[{"role":"user","content":"hello"}]}`,
			expectBodyContains: `openai-response-ok`,
			expectUpstreamPath: "/v1/responses",
			expectUpstreamBody: []string{`"input":[{"content":[{"text":"hello","type":"input_text"}],"role":"user","type":"message"}]`},
			expectAuth:         "Bearer openai-resp-key",
			requests:           openAIRequests,
		},
		{
			name:               "openai responses to openai chat upstream",
			endpoint:           gatewayServer.URL + "/v1/responses",
			payload:            `{"model":"openai-chat-model","input":"hello"}`,
			expectBodyContains: `openai-chat-ok`,
			expectUpstreamPath: "/v1/chat/completions",
			expectUpstreamBody: []string{`"messages":[{"content":"hello","role":"user"}]`},
			expectAuth:         "Bearer openai-chat-key",
			requests:           openAIRequests,
		},
		{
			name:               "anthropic to openai chat upstream",
			endpoint:           gatewayServer.URL + "/v1/messages",
			payload:            `{"model":"openai-chat-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			headers:            map[string]string{"anthropic-version": "2023-06-01"},
			expectBodyContains: `openai-chat-ok`,
			expectUpstreamPath: "/v1/chat/completions",
			expectUpstreamBody: []string{`"messages":[{"content":"hello","role":"user"}]`},
			expectAuth:         "Bearer openai-chat-key",
			requests:           openAIRequests,
		},
		{
			name:               "anthropic to openai responses upstream",
			endpoint:           gatewayServer.URL + "/v1/messages",
			payload:            `{"model":"openai-resp-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			headers:            map[string]string{"anthropic-version": "2023-06-01"},
			expectBodyContains: `openai-response-ok`,
			expectUpstreamPath: "/v1/responses",
			expectUpstreamBody: []string{`"input":[{"content":[{"text":"hello","type":"input_text"}],"role":"user","type":"message"}]`},
			expectAuth:         "Bearer openai-resp-key",
			requests:           openAIRequests,
		},
		{
			name:               "anthropic to gemini upstream",
			endpoint:           gatewayServer.URL + "/v1/messages",
			payload:            `{"model":"gemini-bridge","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			headers:            map[string]string{"anthropic-version": "2023-06-01"},
			expectBodyContains: `gemini-ok`,
			expectUpstreamPath: "/v1beta/models/gem-up:generateContent",
			expectUpstreamBody: []string{`"contents":[{"parts":[{"text":"hello"}],"role":"user"}]`},
			expectAuth:         "gemini-key",
			requests:           geminiRequests,
		},
		{
			name:               "gemini generate to openai chat upstream",
			endpoint:           gatewayServer.URL + "/v1beta/models/openai-chat-model:generateContent",
			payload:            `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expectBodyContains: `openai-chat-ok`,
			expectUpstreamPath: "/v1/chat/completions",
			expectUpstreamBody: []string{`"messages":[{"content":"hello","role":"user"}]`},
			expectAuth:         "Bearer openai-chat-key",
			requests:           openAIRequests,
		},
		{
			name:               "gemini generate to openai responses upstream",
			endpoint:           gatewayServer.URL + "/v1beta/models/openai-resp-model:generateContent",
			payload:            `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expectBodyContains: `openai-response-ok`,
			expectUpstreamPath: "/v1/responses",
			expectUpstreamBody: []string{`"input":[{"content":[{"text":"hello","type":"input_text"}],"role":"user","type":"message"}]`},
			expectAuth:         "Bearer openai-resp-key",
			requests:           openAIRequests,
		},
		{
			name:               "gemini generate to anthropic upstream",
			endpoint:           gatewayServer.URL + "/v1beta/models/claude-bridge:generateContent",
			payload:            `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expectBodyContains: `anthropic-ok`,
			expectUpstreamPath: "/v1/messages",
			expectUpstreamBody: []string{`"messages":[{"content":[{"text":"hello","type":"text"}],"role":"user"}]`},
			expectAuth:         "anthropic-key",
			requests:           anthropicRequests,
		},
		{
			name:               "openai responses stream to anthropic",
			endpoint:           gatewayServer.URL + "/v1/responses",
			payload:            `{"model":"claude-bridge","input":"hello","stream":true}`,
			expectBodyContains: `response.output_text.done`,
			expectUpstreamPath: "/v1/messages",
			expectAuth:         "anthropic-key",
			requests:           anthropicRequests,
		},
		{
			name:               "anthropic stream to openai chat upstream",
			endpoint:           gatewayServer.URL + "/v1/messages",
			payload:            `{"model":"openai-chat-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
			headers:            map[string]string{"anthropic-version": "2023-06-01"},
			expectBodyContains: `content_block_delta`,
			expectUpstreamPath: "/v1/chat/completions",
			expectAuth:         "Bearer openai-chat-key",
			requests:           openAIRequests,
		},
		{
			name:               "gemini stream to openai chat upstream",
			endpoint:           gatewayServer.URL + "/v1beta/models/openai-chat-model:streamGenerateContent",
			payload:            `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			expectBodyContains: `usageMetadata`,
			expectUpstreamPath: "/v1/chat/completions",
			expectAuth:         "Bearer openai-chat-key",
			requests:           openAIRequests,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res, body := postProtocolJSON(t, tc.endpoint, tc.payload, tc.headers)
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("unexpected status: %d body=%s", res.StatusCode, body)
			}
			if !strings.Contains(body, tc.expectBodyContains) {
				t.Fatalf("expected body to contain %q, got %s", tc.expectBodyContains, body)
			}
			seen := <-tc.requests
			if seen.Path != tc.expectUpstreamPath {
				t.Fatalf("unexpected upstream path: %s", seen.Path)
			}
			for _, fragment := range tc.expectUpstreamBody {
				if !strings.Contains(seen.Body, fragment) {
					t.Fatalf("expected upstream body to contain %q, got %s", fragment, seen.Body)
				}
			}
			if strings.HasPrefix(tc.expectAuth, "Bearer ") {
				if seen.Authorization != tc.expectAuth {
					t.Fatalf("unexpected upstream authorization: %s", seen.Authorization)
				}
			} else if seen.APIKey != tc.expectAuth {
				t.Fatalf("unexpected upstream api key: %s", seen.APIKey)
			}
		})
	}
}

func postProtocolJSON(t *testing.T, endpoint string, payload string, headers map[string]string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testGatewaySecret)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s failed: %v", endpoint, err)
	}
	bodyBytes, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	res.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return res, string(bodyBytes)
}
