package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

type bridgeSemanticSummary struct {
	SystemText    string
	UserText      string
	AssistantText string
	ToolCallName  string
	ToolCallID    string
	ToolResultID  string
	ToolResult    string
	ToolCount     int
	Stream        bool
}

func TestDecodeBridgeRequestNormalizesPublicProtocols(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		protocol model.Protocol
		request  parsedProxyRequest
	}{
		{
			name:     "openai chat",
			protocol: model.ProtocolOpenAIChat,
			request: parsedProxyRequest{
				jsonPayload: map[string]any{
					"model":  "alias",
					"stream": true,
					"messages": []any{
						map[string]any{"role": "system", "content": "system text"},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
						map[string]any{
							"role":    "assistant",
							"content": "thinking",
							"tool_calls": []any{
								map[string]any{
									"id":   "call_1",
									"type": "function",
									"function": map[string]any{
										"name":      "lookup",
										"arguments": `{"city":"Paris"}`,
									},
								},
							},
						},
						map[string]any{"role": "tool", "tool_call_id": "call_1", "content": `{"temp":23}`},
					},
					"tools": []any{
						map[string]any{
							"type": "function",
							"function": map[string]any{
								"name":        "lookup",
								"description": "look up weather",
								"parameters":  map[string]any{"type": "object"},
							},
						},
					},
				},
			},
		},
		{
			name:     "openai responses",
			protocol: model.ProtocolOpenAIResponses,
			request: parsedProxyRequest{
				jsonPayload: map[string]any{
					"model":        "alias",
					"instructions": "system text",
					"input": []any{
						map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
						map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "input_text", "text": "thinking"}}},
						map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": map[string]any{"city": "Paris"}},
						map[string]any{"type": "function_call_output", "call_id": "call_1", "output": `{"temp":23}`},
					},
					"tools": []any{
						map[string]any{
							"type":        "function",
							"name":        "lookup",
							"description": "look up weather",
							"parameters":  map[string]any{"type": "object"},
						},
					},
				},
			},
		},
		{
			name:     "anthropic",
			protocol: model.ProtocolAnthropic,
			request: parsedProxyRequest{
				jsonPayload: map[string]any{
					"model":      "alias",
					"max_tokens": 128,
					"system":     "system text",
					"messages": []any{
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
						map[string]any{"role": "assistant", "content": []any{
							map[string]any{"type": "text", "text": "thinking"},
							map[string]any{"type": "tool_use", "id": "call_1", "name": "lookup", "input": map[string]any{"city": "Paris"}},
						}},
						map[string]any{"role": "user", "content": []any{
							map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": `{"temp":23}`},
						}},
					},
					"tools": []any{
						map[string]any{
							"name":         "lookup",
							"description":  "look up weather",
							"input_schema": map[string]any{"type": "object"},
						},
					},
				},
			},
		},
		{
			name:     "gemini",
			protocol: model.ProtocolGeminiGenerate,
			request: parsedProxyRequest{
				rawBody: []byte(`{
					"systemInstruction":{"parts":[{"text":"system text"}]},
					"contents":[
						{"role":"user","parts":[{"text":"hello"}]},
						{"role":"model","parts":[{"text":"thinking"},{"functionCall":{"name":"lookup","args":{"city":"Paris"}}}]},
						{"role":"function","parts":[{"functionResponse":{"name":"lookup","response":{"temp":23}}}]}
					],
					"tools":[{"functionDeclarations":[{"name":"lookup","description":"look up weather","parameters":{"type":"object"}}]}]
				}`),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := decodeBridgeRequest(tc.protocol, tc.request)
			if err != nil {
				t.Fatalf("decode bridge request: %v", err)
			}
			summary := summarizeBridgeRequest(req)
			if summary.SystemText != "system text" {
				t.Fatalf("unexpected system text: %+v", summary)
			}
			if summary.UserText != "hello" {
				t.Fatalf("unexpected user text: %+v", summary)
			}
			if summary.AssistantText != "thinking" {
				t.Fatalf("unexpected assistant text: %+v", summary)
			}
			if summary.ToolCallName != "lookup" {
				t.Fatalf("unexpected tool call summary: %+v", summary)
			}
			if tc.name != "gemini" && summary.ToolCallID != "call_1" {
				t.Fatalf("unexpected tool call id: %+v", summary)
			}
			if tc.name != "gemini" && summary.ToolResultID != "call_1" {
				t.Fatalf("unexpected tool result summary: %+v", summary)
			}
			if !strings.Contains(summary.ToolResult, "temp") {
				t.Fatalf("unexpected tool result payload: %+v", summary)
			}
			if summary.ToolCount != 1 {
				t.Fatalf("expected one tool definition, got %+v", summary)
			}
		})
	}
}

func TestEncodeUpstreamRequestEmitsExpectedShapes(t *testing.T) {
	t.Parallel()

	req := bridgeRequest{
		System: []bridgeContentBlock{{Type: bridgeContentText, Text: "system text"}},
		Messages: []bridgeMessage{
			{Role: "user", Content: []bridgeContentBlock{{Type: bridgeContentText, Text: "hello"}}},
			{Role: "assistant", Content: []bridgeContentBlock{
				{Type: bridgeContentText, Text: "thinking"},
				{Type: bridgeContentToolCall, ToolCallID: "call_1", ToolName: "lookup", Arguments: `{"city":"Paris"}`},
			}},
			{Role: "tool", Content: []bridgeContentBlock{
				{Type: bridgeContentToolResult, ToolCallID: "call_1", Result: `{"temp":23}`},
			}},
		},
		Tools: []bridgeTool{{
			Name:        "lookup",
			Description: "look up weather",
			Parameters:  map[string]any{"type": "object"},
		}},
	}

	cases := []struct {
		name       string
		protocol   model.Protocol
		expectPath string
		assert     func(*testing.T, map[string]any)
	}{
		{
			name:       "openai chat",
			protocol:   model.ProtocolOpenAIChat,
			expectPath: "/v1/chat/completions",
			assert: func(t *testing.T, body map[string]any) {
				if _, ok := body["messages"].([]any); !ok {
					t.Fatalf("expected messages array, got %+v", body)
				}
				firstAssistant := findRoleMessage(body["messages"], "assistant")
				if firstAssistant == nil {
					t.Fatalf("expected assistant message, got %+v", body)
				}
				if _, ok := firstAssistant["tool_calls"].([]any); !ok {
					t.Fatalf("expected tool_calls on assistant message, got %+v", firstAssistant)
				}
			},
		},
		{
			name:       "openai responses",
			protocol:   model.ProtocolOpenAIResponses,
			expectPath: "/v1/responses",
			assert: func(t *testing.T, body map[string]any) {
				input, ok := body["input"].([]any)
				if !ok || len(input) < 3 {
					t.Fatalf("expected responses input items, got %+v", body)
				}
				if !containsResponseItemType(input, "function_call") || !containsResponseItemType(input, "function_call_output") {
					t.Fatalf("expected function_call and function_call_output, got %+v", input)
				}
			},
		},
		{
			name:       "anthropic",
			protocol:   model.ProtocolAnthropic,
			expectPath: "/v1/messages",
			assert: func(t *testing.T, body map[string]any) {
				messages, ok := body["messages"].([]any)
				if !ok || len(messages) < 2 {
					t.Fatalf("expected anthropic messages, got %+v", body)
				}
				bodyText := mustMarshal(messages)
				if !strings.Contains(bodyText, `"tool_use"`) || !strings.Contains(bodyText, `"tool_result"`) {
					t.Fatalf("expected tool_use and tool_result blocks, got %s", bodyText)
				}
			},
		},
		{
			name:       "gemini generate",
			protocol:   model.ProtocolGeminiGenerate,
			expectPath: "/v1beta/models/upstream-model:generateContent",
			assert: func(t *testing.T, body map[string]any) {
				contents, ok := body["contents"].([]any)
				if !ok || len(contents) < 2 {
					t.Fatalf("expected gemini contents, got %+v", body)
				}
				assistant := findRoleMessage(contents, "model")
				if assistant == nil || !strings.Contains(mustMarshal(assistant), `"functionCall"`) {
					t.Fatalf("expected functionCall in model parts, got %+v", assistant)
				}
			},
		},
		{
			name:       "gemini stream",
			protocol:   model.ProtocolGeminiStream,
			expectPath: "/v1beta/models/upstream-model:streamGenerateContent",
			assert: func(t *testing.T, body map[string]any) {
				if _, ok := body["contents"].([]any); !ok {
					t.Fatalf("expected contents array, got %+v", body)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, path, err := encodeUpstreamRequest(tc.protocol, req, "upstream-model")
			if err != nil {
				t.Fatalf("encode upstream request: %v", err)
			}
			if path != tc.expectPath {
				t.Fatalf("unexpected path %q", path)
			}
			var body map[string]any
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				t.Fatalf("unmarshal encoded body: %v\n%s", err, string(bodyBytes))
			}
			tc.assert(t, body)
		})
	}
}

func TestWriteBridgeCompletionProducesPublicResponseModes(t *testing.T) {
	t.Parallel()

	completion := bridgeCompletion{
		ID: "cmp_1",
		Message: bridgeMessage{
			Role: "assistant",
			Content: []bridgeContentBlock{
				{Type: bridgeContentText, Text: "done"},
				{Type: bridgeContentToolCall, ToolCallID: "call_1", ToolName: "lookup", Arguments: `{"city":"Paris"}`},
			},
		},
		FinishReason: "tool_use",
		Usage: model.UsageSummary{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
	}

	cases := []struct {
		name   string
		writer func(*httptest.ResponseRecorder) error
		assert func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name: "openai chat json",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeOpenAIChatJSONCompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				var body map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if body["object"] != "chat.completion" {
					t.Fatalf("unexpected object: %+v", body)
				}
				if !strings.Contains(rr.Body.String(), `"tool_calls"`) {
					t.Fatalf("expected tool_calls in body: %s", rr.Body.String())
				}
			},
		},
		{
			name: "openai chat sse",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeOpenAIChatSSECompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `"chat.completion.chunk"`) || !strings.Contains(rr.Body.String(), `[DONE]`) {
					t.Fatalf("unexpected sse body: %s", rr.Body.String())
				}
			},
		},
		{
			name: "openai responses json",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeOpenAIResponsesJSONCompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `"function_call"`) || !strings.Contains(rr.Body.String(), `"usage"`) {
					t.Fatalf("unexpected responses json: %s", rr.Body.String())
				}
			},
		},
		{
			name: "openai responses sse",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeOpenAIResponsesSSECompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `response.output_item.added`) || !strings.Contains(rr.Body.String(), `response.completed`) {
					t.Fatalf("unexpected responses sse: %s", rr.Body.String())
				}
			},
		},
		{
			name: "anthropic json",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeAnthropicJSONCompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `"tool_use"`) || !strings.Contains(rr.Body.String(), `"stop_reason":"tool_use"`) {
					t.Fatalf("unexpected anthropic json: %s", rr.Body.String())
				}
			},
		},
		{
			name: "anthropic sse",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeAnthropicSSECompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `message_start`) || !strings.Contains(rr.Body.String(), `content_block_start`) || !strings.Contains(rr.Body.String(), `message_stop`) {
					t.Fatalf("unexpected anthropic sse: %s", rr.Body.String())
				}
			},
		},
		{
			name: "gemini json",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeGeminiJSONCompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `"functionCall"`) || !strings.Contains(rr.Body.String(), `"usageMetadata"`) {
					t.Fatalf("unexpected gemini json: %s", rr.Body.String())
				}
			},
		},
		{
			name: "gemini sse",
			writer: func(rr *httptest.ResponseRecorder) error {
				return writeGeminiSSECompletion(rr, completion, "alias")
			},
			assert: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if !strings.Contains(rr.Body.String(), `"usageMetadata"`) || !strings.Contains(rr.Body.String(), `"functionCall"`) {
					t.Fatalf("unexpected gemini sse: %s", rr.Body.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			if err := tc.writer(recorder); err != nil {
				t.Fatalf("write completion: %v", err)
			}
			if recorder.Code != 0 && recorder.Code != 200 {
				t.Fatalf("unexpected status: %d", recorder.Code)
			}
			tc.assert(t, recorder)
		})
	}
}

func summarizeBridgeRequest(req bridgeRequest) bridgeSemanticSummary {
	summary := bridgeSemanticSummary{
		SystemText: bridgePlainText(req.System),
		ToolCount:  len(req.Tools),
		Stream:     req.Stream,
	}
	for _, message := range req.Messages {
		role := normalizeBridgeRole(message.Role)
		switch role {
		case "user":
			if summary.UserText == "" {
				summary.UserText = bridgePlainText(message.Content)
			}
		case "assistant":
			if text := bridgePlainText(message.Content); text != "" && summary.AssistantText == "" {
				summary.AssistantText = text
			}
		}
		for _, block := range message.Content {
			if block.Type == bridgeContentToolCall && summary.ToolCallName == "" {
				summary.ToolCallName = block.ToolName
				summary.ToolCallID = block.ToolCallID
			}
			if block.Type == bridgeContentToolResult && summary.ToolResultID == "" {
				summary.ToolResultID = block.ToolCallID
				summary.ToolResult = firstNonEmpty(block.Result, block.Text)
			}
		}
	}
	return summary
}

func findRoleMessage(items any, role string) map[string]any {
	values, ok := items.([]any)
	if !ok {
		return nil
	}
	for _, item := range values {
		current, ok := item.(map[string]any)
		if ok && current["role"] == role {
			return current
		}
	}
	return nil
}

func containsResponseItemType(items []any, itemType string) bool {
	for _, item := range items {
		current, ok := item.(map[string]any)
		if ok && current["type"] == itemType {
			return true
		}
	}
	return false
}

func mustMarshal(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
