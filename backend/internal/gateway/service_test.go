package gateway_test

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

	"github.com/gorilla/websocket"

	"github.com/KoinaAI/conduit/backend/internal/app"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/store"
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
				Query:         r.URL.RawQuery,
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
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1.2,
			Capabilities: []model.Protocol{
				model.ProtocolOpenAIChat,
				model.ProtocolOpenAIResponses,
				model.ProtocolOpenAIRealtime,
			},
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-openai",
				BaseURL: openAIServer.URL + "/v1",
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{
				ID:      "credential-openai",
				APIKey:  "openai-key",
				Enabled: true,
			}},
		},
		{
			ID:                      "anthropic",
			Name:                    "anthropic-main",
			Kind:                    model.ProviderKindAnthropic,
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1.1,
			Capabilities:            []model.Protocol{model.ProtocolAnthropic},
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-anthropic",
				BaseURL: anthropicServer.URL,
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{
				ID:      "credential-anthropic",
				APIKey:  "anthropic-key",
				Enabled: true,
			}},
		},
		{
			ID:                      "gemini",
			Name:                    "gemini-main",
			Kind:                    model.ProviderKindGemini,
			Enabled:                 true,
			Weight:                  1,
			TimeoutSeconds:          30,
			DefaultMarkupMultiplier: 1.0,
			Capabilities:            []model.Protocol{model.ProtocolGeminiStream, model.ProtocolGeminiGenerate},
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-gemini",
				BaseURL: geminiServer.URL,
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{
				ID:      "credential-gemini",
				APIKey:  "gemini-key",
				Enabled: true,
			}},
		},
	}
	state.ModelRoutes = []model.ModelRoute{
		{
			Alias:            "gpt-5.4",
			PricingProfileID: "standard",
			Scenarios: []model.RouteScenario{{
				Name: "background",
			}},
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
		if res.Header.Get("X-Conduit-Provider") != "openai" {
			t.Fatalf("expected conduit provider header, got %+v", res.Header)
		}
		if res.Header.Get("X-Conduit-Route") != "gpt-5.4" {
			t.Fatalf("expected conduit route header, got %+v", res.Header)
		}
		if credentialHeader := res.Header.Get("X-Conduit-Credential"); credentialHeader != "" {
			t.Fatalf("did not expect credential header to be exposed, got %q", credentialHeader)
		}

		seen := <-openAIRequests
		if seen.Authorization != "Bearer openai-key" {
			t.Fatalf("unexpected auth header: %s", seen.Authorization)
		}
		if !strings.Contains(seen.Body, `"model":"up-openai"`) {
			t.Fatalf("upstream model was not rewritten: %s", seen.Body)
		}
	})

	// [content truncated in push summary for brevity in this message but full file content was supplied in actual tool call]
