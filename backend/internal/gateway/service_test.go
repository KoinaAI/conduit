package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/example/universal-ai-gateway/internal/app"
	"github.com/example/universal-ai-gateway/internal/config"
	"github.com/example/universal-ai-gateway/internal/model"
)

const testGatewaySecret = "test-gateway-secret"

type observedRequest struct {
	Path          string
	Authorization string
	APIKey        string
	Body          string
	Query         string
}

func TestGatewayProtocolsEndToEnd(t *testing.T) {
	openAIRequests := make(chan observedRequest, 8)
	anthropicRequests := make(chan observedRequest, 4)
	geminiRequests := make(chan observedRequest, 4)

	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			openAIRequests <- observedRequest{
				Path:          r.URL.Path,
				Authorization: r.Header.Get("Authorization"),
				Body:          string(body),
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
		case "/v1/responses":
			body, _ := io.ReadAll(r.Body)
			openAIRequests <- observedRequest{
				Path:          r.URL.Path,
				Authorization: r.Header.Get("Authorization"),
				Body:          string(body),
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.created\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
			_, _ = w.Write([]byte("event: response.completed\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":9,\"total_tokens\":14}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n"))
		case "/v1/realtime":
			openAIRequests <- observedRequest{
				Path:          r.URL.Path,
				Authorization: r.Header.Get("Authorization"),
				Query:         r.URL.RawQuery,
			}
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade failed: %v", err)
			}
			defer conn.Close()
			_ = conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"usage": map[string]any{
						"input_tokens":  3,
						"output_tokens": 4,
						"total_tokens":  7,
					},
				},
			})
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
		_, _ = w.Write([]byte(`{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":4}}`))
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"usageMetadata\":{\"promptTokenCount\":6,\"candidatesTokenCount\":8,\"totalTokenCount\":14}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer geminiServer.Close()

	state := model.DefaultState()
	state.PricingProfiles = []model.PricingProfile{
		{
			ID:               "standard",
			Name:             "standard",
			Currency:         "USD",
			InputPerMillion:  1,
			OutputPerMillion: 2,
		},
	}
	state.Providers = []model.Provider{
		{
			ID:                      "openai",
			Name:                    "openai-main",
			Kind:                    model.ProviderKindOpenAICompatible,
			BaseURL:                 openAIServer.URL + "/v1",
			APIKey:                  "openai-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1.2,
			Capabilities: []model.Protocol{
				model.ProtocolOpenAIChat,
				model.ProtocolOpenAIResponses,
				model.ProtocolOpenAIRealtime,
			},
		},
		{
			ID:                      "anthropic",
			Name:                    "anthropic-main",
			Kind:                    model.ProviderKindAnthropic,
			BaseURL:                 anthropicServer.URL,
			APIKey:                  "anthropic-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1.1,
			Capabilities:            []model.Protocol{model.ProtocolAnthropic},
		},
		{
			ID:                      "gemini",
			Name:                    "gemini-main",
			Kind:                    model.ProviderKindGemini,
			BaseURL:                 geminiServer.URL,
			APIKey:                  "gemini-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1.0,
			Capabilities:            []model.Protocol{model.ProtocolGeminiStream, model.ProtocolGeminiGenerate},
		},
	}
	state.ModelRoutes = []model.ModelRoute{
		{
			Alias:            "gpt-5.4",
			PricingProfileID: "standard",
			Targets: []model.RouteTarget{
				{ID: "t1", AccountID: "openai", UpstreamModel: "up-openai", Weight: 1, Enabled: true, MarkupMultiplier: 1.5},
			},
		},
		{
			Alias:            "claude-3.7",
			PricingProfileID: "standard",
			Targets: []model.RouteTarget{
				{ID: "t2", AccountID: "anthropic", UpstreamModel: "claude-up", Weight: 1, Enabled: true, MarkupMultiplier: 1.0},
			},
		},
		{
			Alias:            "gemini-2.5",
			PricingProfileID: "standard",
			Targets: []model.RouteTarget{
				{ID: "t3", AccountID: "gemini", UpstreamModel: "gem-up", Weight: 1, Enabled: true, MarkupMultiplier: 1.0},
			},
		},
	}

	gatewayServer := startGatewayServer(t, state)
	defer gatewayServer.Close()

	t.Run("openai chat", func(t *testing.T) {
		payload := `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`
		res, body := postJSON(t, gatewayServer.URL+"/v1/chat/completions", payload)
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", res.StatusCode, body)
		}

		seen := <-openAIRequests
		if seen.Authorization != "Bearer openai-key" {
			t.Fatalf("unexpected auth header: %s", seen.Authorization)
		}
		if !strings.Contains(seen.Body, `"model":"up-openai"`) {
			t.Fatalf("upstream model was not rewritten: %s", seen.Body)
		}
	})

	t.Run("openai responses sse", func(t *testing.T) {
		payload := `{"model":"gpt-5.4","input":"hello","stream":true}`
		res, body := postJSON(t, gatewayServer.URL+"/v1/responses", payload)
		defer res.Body.Close()

		if !strings.Contains(body, "response.completed") {
			t.Fatalf("expected proxied SSE body, got: %s", body)
		}
		if res.Trailer.Get("X-Gateway-Total-Tokens") != "14" {
			t.Fatalf("expected usage trailer, got %+v", res.Trailer)
		}
		if res.Trailer.Get("X-Gateway-Billing-Final") == "" {
			t.Fatalf("expected billing trailer, got %+v", res.Trailer)
		}
		seen := <-openAIRequests
		if seen.Path != "/v1/responses" {
			t.Fatalf("unexpected observed path: %s", seen.Path)
		}
	})

	t.Run("anthropic messages", func(t *testing.T) {
		payload := `{"model":"claude-3.7","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
		res, _ := postJSON(t, gatewayServer.URL+"/v1/messages", payload)
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status: %d", res.StatusCode)
		}
		seen := <-anthropicRequests
		if seen.APIKey != "anthropic-key" {
			t.Fatalf("expected anthropic api key header, got %s", seen.APIKey)
		}
		if !strings.Contains(seen.Body, `"model":"claude-up"`) {
			t.Fatalf("upstream model was not rewritten: %s", seen.Body)
		}
	})

	t.Run("gemini stream", func(t *testing.T) {
		payload := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
		res, body := postJSON(t, gatewayServer.URL+"/v1beta/models/gemini-2.5:streamGenerateContent", payload)
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("unexpected status: %d", res.StatusCode)
		}
		if !strings.Contains(body, "usageMetadata") {
			t.Fatalf("expected SSE body, got: %s", body)
		}
		seen := <-geminiRequests
		if seen.Path != "/v1beta/models/gem-up:streamGenerateContent" {
			t.Fatalf("unexpected upstream path: %s", seen.Path)
		}
		if seen.APIKey != "gemini-key" {
			t.Fatalf("expected gemini api key header, got: %s", seen.APIKey)
		}
		if seen.Query != "" {
			t.Fatalf("expected gemini request query to stay empty, got: %s", seen.Query)
		}
		if res.Trailer.Get("X-Gateway-Total-Tokens") != "14" {
			t.Fatalf("expected gemini usage trailer, got %+v", res.Trailer)
		}
	})

	t.Run("realtime websocket", func(t *testing.T) {
		wsURL := "ws" + strings.TrimPrefix(gatewayServer.URL, "http") + "/v1/realtime?model=gpt-5.4"
		headers := http.Header{}
		headers.Set("X-API-Key", testGatewaySecret)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
		if err != nil {
			t.Fatalf("dial realtime: %v", err)
		}
		defer conn.Close()

		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("read realtime event: %v", err)
		}
		if event["type"] != "response.completed" {
			t.Fatalf("unexpected realtime event: %+v", event)
		}

		seen := <-openAIRequests
		if !strings.Contains(seen.Query, "model=up-openai") {
			t.Fatalf("expected rewritten realtime query: %s", seen.Query)
		}
	})

	t.Run("realtime rejects disallowed browser origin", func(t *testing.T) {
		wsURL := "ws" + strings.TrimPrefix(gatewayServer.URL, "http") + "/v1/realtime?model=gpt-5.4"
		headers := http.Header{}
		headers.Set("X-API-Key", testGatewaySecret)
		headers.Set("Origin", "https://evil.example")
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
		if err == nil {
			conn.Close()
			t.Fatal("expected websocket origin check to reject the request")
		}
	})

	t.Run("admin state reflects history", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, gatewayServer.URL+"/api/admin/state", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Admin-Token", "admin-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("admin request failed: %v", err)
		}
		defer res.Body.Close()
		var saved model.State
		if err := json.NewDecoder(res.Body).Decode(&saved); err != nil {
			t.Fatalf("decode state: %v", err)
		}
		if len(saved.RequestHistory) < 4 {
			t.Fatalf("expected request history to be populated, got %d", len(saved.RequestHistory))
		}
	})
}

func TestGatewayFallsBackToRouteAliasWhenUpstreamModelIsBlank(t *testing.T) {
	t.Parallel()

	requests := make(chan observedRequest, 1)
	openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- observedRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			Body:          string(body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer openAIServer.Close()

	state := model.DefaultState()
	state.Providers = []model.Provider{
		{
			ID:                      "openai",
			Name:                    "openai-main",
			Kind:                    model.ProviderKindOpenAICompatible,
			BaseURL:                 openAIServer.URL + "/v1",
			APIKey:                  "openai-key",
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1,
			Capabilities:            []model.Protocol{model.ProtocolOpenAIChat},
		},
	}
	state.ModelRoutes = []model.ModelRoute{
		{
			Alias: "gpt-5.4",
			Targets: []model.RouteTarget{
				{ID: "t1", AccountID: "openai", UpstreamModel: "", Weight: 1, Enabled: true, MarkupMultiplier: 1},
			},
		},
	}

	gatewayServer := startGatewayServer(t, state)
	defer gatewayServer.Close()

	res, body := postJSON(t, gatewayServer.URL+"/v1/chat/completions", `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", res.StatusCode, body)
	}

	seen := <-requests
	if !strings.Contains(seen.Body, `"model":"gpt-5.4"`) {
		t.Fatalf("expected route alias fallback when upstream model is blank, got %s", seen.Body)
	}
}

func startGatewayServer(t *testing.T, state model.State) *httptest.Server {
	t.Helper()

	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "state.json")
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	cfg := config.Config{
		BindAddress:         ":0",
		StatePath:           statePath,
		AdminToken:          "admin-token",
		EnableRealtime:      true,
		RequestHistory:      20,
		BootstrapGatewayKey: testGatewaySecret,
	}
	instance, err := app.New(cfg)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return httptest.NewServer(instance.Handler())
}

func postJSON(t *testing.T, endpoint string, payload string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("new request failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testGatewaySecret)
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
