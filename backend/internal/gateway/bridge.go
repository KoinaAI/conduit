package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

type bridgeContentType string

const (
	bridgeContentText       bridgeContentType = "text"
	bridgeContentImage      bridgeContentType = "image"
	bridgeContentToolCall   bridgeContentType = "tool_call"
	bridgeContentToolResult bridgeContentType = "tool_result"
)

type bridgeContentBlock struct {
	Type       bridgeContentType
	Text       string
	URL        string
	MIMEType   string
	Data       string
	ToolCallID string
	ToolName   string
	Arguments  string
	Result     string
	IsError    bool
}

type bridgeMessage struct {
	Role    string
	Content []bridgeContentBlock
}

type bridgeTool struct {
	Name        string
	Description string
	Parameters  any
}

type bridgeRequest struct {
	System      []bridgeContentBlock
	Messages    []bridgeMessage
	Tools       []bridgeTool
	Stream      bool
	MaxTokens   any
	Temperature any
	TopP        any
}

type bridgeCompletion struct {
	ID           string
	Message      bridgeMessage
	FinishReason string
	Usage        model.UsageSummary
}

func resolveUpstreamProtocol(publicProtocol model.Protocol, requestStream bool, provider model.Provider) (model.Protocol, bool) {
	switch provider.Kind {
	case model.ProviderKindOpenAICompatible:
		return resolveOpenAICompatibleUpstream(publicProtocol, provider)
	case model.ProviderKindAnthropic:
		if canBridgeViaAnthropic(publicProtocol) && provider.Supports(model.ProtocolAnthropic) {
			return model.ProtocolAnthropic, true
		}
	case model.ProviderKindGemini:
		if !canBridgeViaGemini(publicProtocol) {
			return "", false
		}
		if publicProtocol == model.ProtocolGeminiStream && provider.Supports(model.ProtocolGeminiStream) {
			return model.ProtocolGeminiStream, true
		}
		if publicProtocol == model.ProtocolGeminiGenerate && provider.Supports(model.ProtocolGeminiGenerate) {
			return model.ProtocolGeminiGenerate, true
		}
		if provider.Supports(model.ProtocolGeminiGenerate) {
			return model.ProtocolGeminiGenerate, true
		}
		if requestStream && provider.Supports(model.ProtocolGeminiStream) {
			return model.ProtocolGeminiStream, true
		}
		if provider.Supports(model.ProtocolGeminiStream) {
			return model.ProtocolGeminiStream, true
		}
	}
	return "", false
}

func resolveOpenAICompatibleUpstream(publicProtocol model.Protocol, provider model.Provider) (model.Protocol, bool) {
	hasChat := provider.Supports(model.ProtocolOpenAIChat)
	hasResponses := provider.Supports(model.ProtocolOpenAIResponses)

	switch publicProtocol {
	case model.ProtocolOpenAIRealtime:
		if provider.Supports(model.ProtocolOpenAIRealtime) {
			return model.ProtocolOpenAIRealtime, true
		}
	case model.ProtocolOpenAIChat:
		if hasChat {
			return model.ProtocolOpenAIChat, true
		}
		if hasResponses {
			return model.ProtocolOpenAIResponses, true
		}
	case model.ProtocolOpenAIResponses:
		if hasResponses {
			return model.ProtocolOpenAIResponses, true
		}
		if hasChat {
			return model.ProtocolOpenAIChat, true
		}
	case model.ProtocolAnthropic, model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		if hasResponses {
			return model.ProtocolOpenAIResponses, true
		}
		if hasChat {
			return model.ProtocolOpenAIChat, true
		}
	}
	return "", false
}

func canBridgeViaAnthropic(publicProtocol model.Protocol) bool {
	switch publicProtocol {
	case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses, model.ProtocolAnthropic, model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		return true
	default:
		return false
	}
}

func canBridgeViaGemini(publicProtocol model.Protocol) bool {
	switch publicProtocol {
	case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses, model.ProtocolAnthropic, model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		return true
	default:
		return false
	}
}

func decodeBridgeRequest(publicProtocol model.Protocol, request parsedProxyRequest) (bridgeRequest, error) {
	switch publicProtocol {
	case model.ProtocolOpenAIChat:
		return decodeOpenAIChatRequest(request.jsonPayload)
	case model.ProtocolOpenAIResponses:
		return decodeOpenAIResponsesRequest(request.jsonPayload)
	case model.ProtocolAnthropic:
		return decodeAnthropicRequest(request.jsonPayload)
	case model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		return decodeGeminiRequest(request.rawBody, publicProtocol == model.ProtocolGeminiStream)
	default:
		return bridgeRequest{}, fmt.Errorf("unsupported public protocol %s", publicProtocol)
	}
}

func decodeOpenAIChatRequest(payload map[string]any) (bridgeRequest, error) {
	req := bridgeRequest{}
	if payload == nil {
		return req, nil
	}
	req.Stream, _ = payload["stream"].(bool)
	req.MaxTokens = firstPresent(payload, "max_tokens", "max_completion_tokens")
	req.Temperature = payload["temperature"]
	req.TopP = payload["top_p"]
	req.Tools = decodeOpenAITools(payload["tools"])

	if messages, ok := payload["messages"].([]any); ok {
		for _, item := range messages {
			current, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role := normalizeBridgeRole(readStringMap(current, "role"))
			content := decodeOpenAIContentBlocks(current["content"])
			if role == "system" {
				req.System = append(req.System, content...)
				continue
			}
			if role == "assistant" {
				content = append(content, decodeOpenAIToolCallBlocks(current["tool_calls"])...)
			}
			if role == "tool" {
				content = decodeOpenAIToolResultBlocks(readStringAny(current["tool_call_id"]), current["content"])
			}
			req.Messages = append(req.Messages, bridgeMessage{
				Role:    role,
				Content: content,
			})
		}
	}
	return req, nil
}

func decodeOpenAIResponsesRequest(payload map[string]any) (bridgeRequest, error) {
	req := bridgeRequest{}
	if payload == nil {
		return req, nil
	}
	req.Stream, _ = payload["stream"].(bool)
	req.MaxTokens = firstPresent(payload, "max_output_tokens", "max_tokens")
	req.Temperature = payload["temperature"]
	req.TopP = payload["top_p"]
	req.Tools = decodeOpenAITools(payload["tools"])
	if instructions, _ := payload["instructions"].(string); strings.TrimSpace(instructions) != "" {
		req.System = append(req.System, bridgeContentBlock{Type: bridgeContentText, Text: strings.TrimSpace(instructions)})
	}
	appendResponsesInputMessages(&req.Messages, payload["input"])
	return req, nil
}

func decodeAnthropicRequest(payload map[string]any) (bridgeRequest, error) {
	req := bridgeRequest{}
	if payload == nil {
		return req, nil
	}
	req.Stream, _ = payload["stream"].(bool)
	req.MaxTokens = payload["max_tokens"]
	req.Temperature = payload["temperature"]
	req.TopP = payload["top_p"]
	req.System = decodeAnthropicSystemBlocks(payload["system"])
	if items, ok := payload["messages"].([]any); ok {
		for _, item := range items {
			current, ok := item.(map[string]any)
			if !ok {
				continue
			}
			req.Messages = append(req.Messages, bridgeMessage{
				Role:    normalizeBridgeRole(readStringMap(current, "role")),
				Content: decodeAnthropicContentBlocks(current["content"]),
			})
		}
	}
	if tools, ok := payload["tools"].([]any); ok {
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			req.Tools = append(req.Tools, bridgeTool{
				Name:        readStringMap(tool, "name"),
				Description: readStringMap(tool, "description"),
				Parameters:  tool["input_schema"],
			})
		}
	}
	return req, nil
}

func decodeGeminiRequest(rawBody []byte, stream bool) (bridgeRequest, error) {
	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return bridgeRequest{}, fmt.Errorf("request body must be JSON: %w", err)
	}
	req := bridgeRequest{Stream: stream}
	if systemInstruction, ok := payload["systemInstruction"].(map[string]any); ok {
		req.System = append(req.System, decodeGeminiParts(systemInstruction["parts"])...)
	} else if systemInstruction, ok := payload["system_instruction"].(map[string]any); ok {
		req.System = append(req.System, decodeGeminiParts(systemInstruction["parts"])...)
	}
	if contents, ok := payload["contents"].([]any); ok {
		for _, item := range contents {
			content, ok := item.(map[string]any)
			if !ok {
				continue
			}
			req.Messages = append(req.Messages, bridgeMessage{
				Role:    normalizeGeminiRole(readStringMap(content, "role")),
				Content: decodeGeminiParts(content["parts"]),
			})
		}
	}
	if generationConfig, ok := payload["generationConfig"].(map[string]any); ok {
		req.MaxTokens = generationConfig["maxOutputTokens"]
		req.Temperature = generationConfig["temperature"]
		req.TopP = generationConfig["topP"]
	}
	if tools, ok := payload["tools"].([]any); ok {
		for _, toolItem := range tools {
			toolMap, ok := toolItem.(map[string]any)
			if !ok {
				continue
			}
			declarations, ok := toolMap["functionDeclarations"].([]any)
			if !ok {
				continue
			}
			for _, item := range declarations {
				decl, ok := item.(map[string]any)
				if !ok {
					continue
				}
				req.Tools = append(req.Tools, bridgeTool{
					Name:        readStringMap(decl, "name"),
					Description: readStringMap(decl, "description"),
					Parameters:  firstPresent(decl, "parameters", "parametersJsonSchema"),
				})
			}
		}
	}
	return req, nil
}

func encodeUpstreamRequest(upstreamProtocol model.Protocol, req bridgeRequest, upstreamModel string) ([]byte, string, error) {
	switch upstreamProtocol {
	case model.ProtocolOpenAIChat:
		body, err := json.Marshal(buildOpenAIChatPayload(req, upstreamModel, false))
		return body, "/v1/chat/completions", err
	case model.ProtocolOpenAIResponses:
		body, err := json.Marshal(buildOpenAIResponsesPayload(req, upstreamModel, false))
		return body, "/v1/responses", err
	case model.ProtocolAnthropic:
		body, err := json.Marshal(buildAnthropicPayload(req, upstreamModel, false))
		return body, "/v1/messages", err
	case model.ProtocolGeminiGenerate:
		body, err := json.Marshal(buildGeminiPayload(req))
		return body, "/v1beta/models/" + upstreamModel + ":generateContent", err
	case model.ProtocolGeminiStream:
		body, err := json.Marshal(buildGeminiPayload(req))
		return body, "/v1beta/models/" + upstreamModel + ":streamGenerateContent", err
	default:
		return nil, "", fmt.Errorf("unsupported upstream protocol %s", upstreamProtocol)
	}
}

func buildOpenAIChatPayload(req bridgeRequest, upstreamModel string, stream bool) map[string]any {
	out := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = req.TopP
	}

	messages := make([]map[string]any, 0, len(req.Messages)+1)
	if system := encodeOpenAIChatSystemMessage(req.System); system != nil {
		messages = append(messages, system)
	}
	for _, message := range req.Messages {
		switch normalizeBridgeRole(message.Role) {
		case "tool":
			messages = append(messages, encodeOpenAIChatToolMessages(message.Content)...)
		default:
			if encoded := encodeOpenAIChatConversationMessage(message); encoded != nil {
				messages = append(messages, encoded)
			}
		}
	}
	out["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
					"parameters":  tool.Parameters,
				},
			})
		}
		out["tools"] = tools
	}
	return out
}

func buildOpenAIResponsesPayload(req bridgeRequest, upstreamModel string, stream bool) map[string]any {
	out := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}
	if instructions := bridgePlainText(req.System); instructions != "" {
		out["instructions"] = instructions
	}
	if req.MaxTokens != nil {
		out["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = req.TopP
	}

	input := make([]map[string]any, 0, len(req.Messages)+1)
	for _, message := range req.Messages {
		encoded := encodeOpenAIResponsesInput(message)
		input = append(input, encoded...)
	}
	if len(input) == 0 {
		input = append(input, map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{
					"type": "input_text",
					"text": "",
				},
			},
		})
	}
	out["input"] = input
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			})
		}
		out["tools"] = tools
	}
	return out
}

func buildAnthropicPayload(req bridgeRequest, upstreamModel string, stream bool) map[string]any {
	out := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}
	if system := bridgePlainText(req.System); system != "" {
		out["system"] = system
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = req.MaxTokens
	} else {
		out["max_tokens"] = 1024
	}
	if req.Temperature != nil {
		out["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = req.TopP
	}

	messages := make([]map[string]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		encoded := encodeAnthropicMessage(message)
		if encoded != nil {
			messages = append(messages, encoded)
		}
	}
	out["messages"] = messages
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": tool.Parameters,
			})
		}
		out["tools"] = tools
	}
	return out
}

func buildGeminiPayload(req bridgeRequest) map[string]any {
	out := map[string]any{}
	if parts := encodeGeminiParts(req.System); len(parts) > 0 {
		out["systemInstruction"] = map[string]any{
			"parts": parts,
		}
	}
	contents := make([]map[string]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		encoded := encodeGeminiMessage(message)
		if encoded != nil {
			contents = append(contents, encoded)
		}
	}
	out["contents"] = contents
	generationConfig := map[string]any{}
	if req.MaxTokens != nil {
		generationConfig["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		generationConfig["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		generationConfig["topP"] = req.TopP
	}
	if len(generationConfig) > 0 {
		out["generationConfig"] = generationConfig
	}
	if len(req.Tools) > 0 {
		functions := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			functions = append(functions, map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			})
		}
		out["tools"] = []map[string]any{{"functionDeclarations": functions}}
	}
	return out
}

func parseBridgeCompletion(resp *http.Response, observer *UsageObserver, upstreamProtocol model.Protocol) (bridgeCompletion, error) {
	payload, err := ioReadAllLimited(resp.Body)
	if err != nil {
		return bridgeCompletion{}, err
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		observer.ObserveJSON(payload)
		return parseBridgeJSONCompletion(upstreamProtocol, payload, observer.Summary())
	}
	if upstreamProtocol == model.ProtocolGeminiStream || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		scanner := bufio.NewScanner(bytes.NewReader(payload))
		scanner.Buffer(make([]byte, 0, 16<<10), 1<<20)
		for scanner.Scan() {
			observer.ObserveLine([]byte(scanner.Text()))
		}
		if err := scanner.Err(); err != nil {
			return bridgeCompletion{}, err
		}
		return parseBridgeStreamingCompletion(upstreamProtocol, payload, observer.Summary())
	}
	observer.ObserveJSON(payload)
	return parseBridgeJSONCompletion(upstreamProtocol, payload, observer.Summary())
}

func parseBridgeJSONCompletion(upstreamProtocol model.Protocol, payload []byte, usage model.UsageSummary) (bridgeCompletion, error) {
	switch upstreamProtocol {
	case model.ProtocolOpenAIChat:
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return bridgeCompletion{}, err
		}
		return bridgeCompletion{
			ID:           readStringMap(body, "id"),
			Message:      parseOpenAIChatCompletionMessage(body),
			FinishReason: firstChoiceFinishReasonMap(body),
			Usage:        usage,
		}, nil
	case model.ProtocolOpenAIResponses:
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return bridgeCompletion{}, err
		}
		return bridgeCompletion{
			ID:           readStringMap(body, "id"),
			Message:      parseOpenAIResponsesCompletionMessage(body),
			FinishReason: mapOpenAIResponsesStatusToFinish(readStringMap(body, "status")),
			Usage:        usage,
		}, nil
	case model.ProtocolAnthropic:
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return bridgeCompletion{}, err
		}
		return bridgeCompletion{
			ID: readStringMap(body, "id"),
			Message: bridgeMessage{
				Role:    "assistant",
				Content: decodeAnthropicContentBlocks(body["content"]),
			},
			FinishReason: readStringMap(body, "stop_reason"),
			Usage:        usage,
		}, nil
	case model.ProtocolGeminiGenerate:
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			return bridgeCompletion{}, err
		}
		return bridgeCompletion{
			ID:           model.NewID("gem"),
			Message:      parseGeminiCompletionMessage(body),
			FinishReason: extractGeminiFinishReason(body),
			Usage:        usage,
		}, nil
	default:
		return bridgeCompletion{}, fmt.Errorf("unsupported upstream protocol %s", upstreamProtocol)
	}
}

func parseBridgeStreamingCompletion(upstreamProtocol model.Protocol, payload []byte, usage model.UsageSummary) (bridgeCompletion, error) {
	switch upstreamProtocol {
	case model.ProtocolGeminiStream:
		return parseGeminiStreamingCompletion(payload, usage)
	default:
		return bridgeCompletion{}, fmt.Errorf("streaming completion parser is not available for %s", upstreamProtocol)
	}
}

func parseGeminiStreamingCompletion(payload []byte, usage model.UsageSummary) (bridgeCompletion, error) {
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 0, 16<<10), 1<<20)

	message := bridgeMessage{Role: "assistant"}
	finishReason := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			return bridgeCompletion{}, err
		}
		parts := parseGeminiCompletionMessage(chunk).Content
		if len(parts) > 0 {
			message.Content = append(message.Content, parts...)
		}
		if value := extractGeminiFinishReason(chunk); value != "" {
			finishReason = value
		}
	}
	if err := scanner.Err(); err != nil {
		return bridgeCompletion{}, err
	}
	return bridgeCompletion{
		ID:           model.NewID("gem"),
		Message:      coalesceTextBlocks(message),
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

func writeBridgeCompletion(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, exchange upstreamExchange, publicAlias string) error {
	completion, err := parseBridgeCompletion(resp, observer, exchange.UpstreamProtocol)
	if err != nil {
		return err
	}
	completion.Message = coalesceTextBlocks(completion.Message)
	switch exchange.ResponseMode {
	case responseModeOpenAIChatJSON:
		return writeOpenAIChatJSONCompletion(w, completion, publicAlias)
	case responseModeOpenAIChatSSE:
		return writeOpenAIChatSSECompletion(w, completion, publicAlias)
	case responseModeOpenAIResponsesJSON:
		return writeOpenAIResponsesJSONCompletion(w, completion, publicAlias)
	case responseModeOpenAIResponsesSSE:
		return writeOpenAIResponsesSSECompletion(w, completion, publicAlias)
	case responseModeAnthropicJSON:
		return writeAnthropicJSONCompletion(w, completion, publicAlias)
	case responseModeAnthropicSSE:
		return writeAnthropicSSECompletion(w, completion, publicAlias)
	case responseModeGeminiJSON:
		return writeGeminiJSONCompletion(w, completion, publicAlias)
	case responseModeGeminiSSE:
		return writeGeminiSSECompletion(w, completion, publicAlias)
	default:
		return fmt.Errorf("unsupported response mode %q", exchange.ResponseMode)
	}
}

func writeOpenAIChatJSONCompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	message := encodeOpenAIChatAssistantMessage(completion.Message.Content)
	body := map[string]any{
		"id":      fallbackID(completion.ID, "chatcmpl"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   publicAlias,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": mapOpenAIFinishReason(completion.FinishReason),
			},
		},
		"usage": openAIUsageMap(completion.Usage),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(body)
}

func writeOpenAIChatSSECompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	responseID := fallbackID(completion.ID, "chatcmpl")
	if err := writeSSEData(w, map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   publicAlias,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
				},
				"finish_reason": nil,
			},
		},
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	if text := bridgePlainText(completion.Message.Content); text != "" {
		if err := writeSSEData(w, map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   publicAlias,
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"content": text,
					},
					"finish_reason": nil,
				},
			},
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	if toolCalls := encodeOpenAIToolCalls(completion.Message.Content); len(toolCalls) > 0 {
		if err := writeSSEData(w, map[string]any{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   publicAlias,
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": toolCalls,
					},
					"finish_reason": nil,
				},
			},
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	if err := writeSSEData(w, map[string]any{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   publicAlias,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": mapOpenAIFinishReason(completion.FinishReason),
			},
		},
		"usage": openAIUsageMap(completion.Usage),
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeOpenAIResponsesJSONCompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	now := time.Now().Unix()
	output := buildOpenAIResponsesOutput(completion.Message.Content)
	body := map[string]any{
		"id":                  fallbackID(completion.ID, "resp"),
		"object":              "response",
		"created_at":          now,
		"completed_at":        now,
		"status":              "completed",
		"error":               nil,
		"model":               publicAlias,
		"output":              output,
		"usage":               openAIResponsesUsageMap(completion.Usage),
		"text":                map[string]any{"format": map[string]any{"type": "text"}},
		"parallel_tool_calls": true,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(body)
}

func writeOpenAIResponsesSSECompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	responseID := fallbackID(completion.ID, "resp")
	createdAt := time.Now().Unix()
	output := buildOpenAIResponsesOutput(completion.Message.Content)

	if err := writeSSEEvent(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      publicAlias,
			"output":     []any{},
		},
		"sequence_number": 0,
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}

	sequence := 1
	for index, item := range output {
		itemID := readStringMap(item, "id")
		if err := writeSSEEvent(w, "response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"output_index":    index,
			"sequence_number": sequence,
			"item":            item,
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
		sequence++

		if readStringMap(item, "type") == "message" {
			content, _ := item["content"].([]map[string]any)
			if len(content) == 0 {
				if raw, ok := item["content"].([]any); ok {
					content = make([]map[string]any, 0, len(raw))
					for _, entry := range raw {
						block, ok := entry.(map[string]any)
						if ok {
							content = append(content, block)
						}
					}
				}
			}
			if len(content) > 0 {
				part := content[0]
				if err := writeSSEEvent(w, "response.content_part.added", map[string]any{
					"type":            "response.content_part.added",
					"content_index":   0,
					"item_id":         itemID,
					"output_index":    index,
					"sequence_number": sequence,
					"part": map[string]any{
						"type":        part["type"],
						"text":        "",
						"annotations": []any{},
					},
				}); err != nil {
					return proxyResponseWriteError{err: err}
				}
				sequence++
				if text, ok := part["text"].(string); ok && text != "" {
					if err := writeSSEEvent(w, "response.output_text.delta", map[string]any{
						"type":            "response.output_text.delta",
						"content_index":   0,
						"item_id":         itemID,
						"output_index":    index,
						"sequence_number": sequence,
						"delta":           text,
						"logprobs":        []any{},
					}); err != nil {
						return proxyResponseWriteError{err: err}
					}
					sequence++
					if err := writeSSEEvent(w, "response.output_text.done", map[string]any{
						"type":            "response.output_text.done",
						"content_index":   0,
						"item_id":         itemID,
						"output_index":    index,
						"sequence_number": sequence,
						"text":            text,
						"logprobs":        []any{},
					}); err != nil {
						return proxyResponseWriteError{err: err}
					}
					sequence++
				}
				if err := writeSSEEvent(w, "response.content_part.done", map[string]any{
					"type":            "response.content_part.done",
					"content_index":   0,
					"item_id":         itemID,
					"output_index":    index,
					"sequence_number": sequence,
					"part":            part,
				}); err != nil {
					return proxyResponseWriteError{err: err}
				}
				sequence++
			}
		}

		if err := writeSSEEvent(w, "response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"output_index":    index,
			"sequence_number": sequence,
			"item":            item,
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
		sequence++
	}

	if err := writeSSEEvent(w, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":           responseID,
			"object":       "response",
			"created_at":   createdAt,
			"completed_at": time.Now().Unix(),
			"status":       "completed",
			"model":        publicAlias,
			"output":       output,
			"usage":        openAIResponsesUsageMap(completion.Usage),
		},
		"sequence_number": sequence,
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeAnthropicJSONCompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	body := map[string]any{
		"id":          fallbackID(completion.ID, "msg"),
		"type":        "message",
		"role":        "assistant",
		"model":       publicAlias,
		"content":     encodeAnthropicAssistantContent(completion.Message.Content),
		"stop_reason": mapAnthropicStopReason(completion.FinishReason),
		"usage": map[string]any{
			"input_tokens":  completion.Usage.InputTokens,
			"output_tokens": completion.Usage.OutputTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(body)
}

func writeAnthropicSSECompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	messageID := fallbackID(completion.ID, "msg")
	if err := writeSSEEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         publicAlias,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": completion.Usage.InputTokens, "output_tokens": 0},
		},
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}

	for index, block := range completion.Message.Content {
		switch block.Type {
		case bridgeContentText:
			if err := writeSSEEvent(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return proxyResponseWriteError{err: err}
			}
			if block.Text != "" {
				if err := writeSSEEvent(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{"type": "text_delta", "text": block.Text},
				}); err != nil {
					return proxyResponseWriteError{err: err}
				}
			}
			if err := writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return proxyResponseWriteError{err: err}
			}
		case bridgeContentToolCall:
			if err := writeSSEEvent(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    fallbackID(block.ToolCallID, "toolu"),
					"name":  block.ToolName,
					"input": decodeJSONStringOrFallback(block.Arguments, map[string]any{}),
				},
			}); err != nil {
				return proxyResponseWriteError{err: err}
			}
			if err := writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
				return proxyResponseWriteError{err: err}
			}
		}
	}

	if err := writeSSEEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   mapAnthropicStopReason(completion.FinishReason),
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": completion.Usage.OutputTokens,
		},
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	if err := writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeGeminiJSONCompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	body := map[string]any{
		"candidates": []map[string]any{
			{
				"index": 0,
				"content": map[string]any{
					"role":  "model",
					"parts": encodeGeminiParts(completion.Message.Content),
				},
				"finishReason": mapGeminiFinishReason(completion.FinishReason),
			},
		},
		"usageMetadata": usageSummaryMap(completion.Usage),
		"modelVersion":  publicAlias,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(body)
}

func writeGeminiSSECompletion(w http.ResponseWriter, completion bridgeCompletion, publicAlias string) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if len(completion.Message.Content) > 0 {
		if err := writeSSEData(w, map[string]any{
			"candidates": []map[string]any{
				{
					"index": 0,
					"content": map[string]any{
						"role":  "model",
						"parts": encodeGeminiParts(completion.Message.Content),
					},
				},
			},
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	if err := writeSSEData(w, map[string]any{
		"candidates": []map[string]any{
			{
				"index":        0,
				"content":      map[string]any{"role": "model", "parts": []map[string]any{}},
				"finishReason": mapGeminiFinishReason(completion.FinishReason),
			},
		},
		"usageMetadata": usageSummaryMap(completion.Usage),
		"modelVersion":  publicAlias,
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func openAIUsageMap(summary model.UsageSummary) map[string]any {
	return map[string]any{
		"prompt_tokens":     summary.InputTokens,
		"completion_tokens": summary.OutputTokens,
		"total_tokens":      summary.TotalTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": summary.CachedInputTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": summary.ReasoningTokens,
		},
	}
}

func openAIResponsesUsageMap(summary model.UsageSummary) map[string]any {
	return map[string]any{
		"input_tokens": summary.InputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": summary.CachedInputTokens,
		},
		"output_tokens": summary.OutputTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": summary.ReasoningTokens,
		},
		"total_tokens": summary.TotalTokens,
	}
}

func mapOpenAIFinishReason(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "stop", "end_turn", "stop_sequence", "stopsequence", "stop_sequence_reached":
		return "stop"
	case "length", "max_tokens", "max_output_tokens":
		return "length"
	case "tool_use", "tool_calls", "function_call":
		return "tool_calls"
	case "content_filter", "safety", "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func mapOpenAIResponsesStatusToFinish(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "completed") {
		return "stop"
	}
	return strings.TrimSpace(value)
}

func normalizeBridgeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant", "model":
		return "assistant"
	case "system":
		return "system"
	case "tool", "function":
		return "tool"
	default:
		return "user"
	}
}

func normalizeGeminiRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "model":
		return "assistant"
	case "function", "tool":
		return "tool"
	default:
		return "user"
	}
}

func decodeOpenAITools(value any) []bridgeTool {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	tools := make([]bridgeTool, 0, len(items))
	for _, item := range items {
		current, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function := current
		if nested, ok := current["function"].(map[string]any); ok {
			function = nested
		}
		tools = append(tools, bridgeTool{
			Name:        readStringMap(function, "name"),
			Description: readStringMap(function, "description"),
			Parameters:  firstPresent(function, "parameters", "input_schema"),
		})
	}
	return tools
}

func appendResponsesInputMessages(messages *[]bridgeMessage, value any) {
	switch current := value.(type) {
	case string:
		*messages = append(*messages, bridgeMessage{
			Role:    "user",
			Content: []bridgeContentBlock{{Type: bridgeContentText, Text: current}},
		})
	case []any:
		for _, item := range current {
			appendResponsesInputMessages(messages, item)
		}
	case map[string]any:
		itemType := readStringMap(current, "type")
		switch {
		case itemType == "message" || readStringMap(current, "role") != "":
			*messages = append(*messages, bridgeMessage{
				Role:    normalizeBridgeRole(readStringMap(current, "role")),
				Content: decodeResponsesContent(current["content"]),
			})
		case itemType == "function_call":
			*messages = append(*messages, bridgeMessage{
				Role: "assistant",
				Content: []bridgeContentBlock{{
					Type:       bridgeContentToolCall,
					ToolCallID: firstNonEmpty(readStringMap(current, "call_id"), readStringMap(current, "id")),
					ToolName:   readStringMap(current, "name"),
					Arguments:  stringifyStructuredValue(current["arguments"]),
				}},
			})
		case itemType == "function_call_output":
			*messages = append(*messages, bridgeMessage{
				Role: "tool",
				Content: []bridgeContentBlock{{
					Type:       bridgeContentToolResult,
					ToolCallID: readStringMap(current, "call_id"),
					Result:     stringifyLooseValue(current["output"]),
				}},
			})
		default:
			if text := flattenResponsesContent(current); text != "" {
				*messages = append(*messages, bridgeMessage{
					Role:    "user",
					Content: []bridgeContentBlock{{Type: bridgeContentText, Text: text}},
				})
			}
		}
	}
}

func flattenResponsesContent(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case []any:
		parts := make([]string, 0, len(current))
		for _, item := range current {
			if text := flattenResponsesContent(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		itemType := readStringMap(current, "type")
		switch itemType {
		case "", "text", "input_text", "output_text":
			if text, ok := current["text"].(string); ok {
				return text
			}
		}
		if text, ok := current["content"].(string); ok {
			return text
		}
	}
	return ""
}

func appendParagraph(base, next string) string {
	base = strings.TrimSpace(base)
	next = strings.TrimSpace(next)
	switch {
	case base == "":
		return next
	case next == "":
		return base
	default:
		return base + "\n\n" + next
	}
}

func readStringMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

func readStringAny(value any) string {
	if current, ok := value.(string); ok {
		return current
	}
	return ""
}

func firstPresent(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if values == nil {
			continue
		}
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func ioReadAllLimited(body io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(body, 4<<20))
}

func buildGeminiPath(upstreamModel string, stream bool) string {
	if stream {
		return "/v1beta/models/" + upstreamModel + ":streamGenerateContent"
	}
	return "/v1beta/models/" + upstreamModel + ":generateContent"
}

func decodeOpenAIContentBlocks(value any) []bridgeContentBlock {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []bridgeContentBlock{{Type: bridgeContentText, Text: current}}
	case []any:
		blocks := make([]bridgeContentBlock, 0, len(current))
		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blocks = append(blocks, decodeOpenAIContentBlock(block)...)
		}
		return blocks
	case map[string]any:
		return decodeOpenAIContentBlock(current)
	default:
		return nil
	}
}

func decodeOpenAIContentBlock(block map[string]any) []bridgeContentBlock {
	blockType := readStringMap(block, "type")
	switch blockType {
	case "", "text", "input_text", "output_text":
		if text := readStringMap(block, "text"); text != "" {
			return []bridgeContentBlock{{Type: bridgeContentText, Text: text}}
		}
	case "image_url", "input_image":
		switch value := block["image_url"].(type) {
		case string:
			return []bridgeContentBlock{{Type: bridgeContentImage, URL: value}}
		case map[string]any:
			if url := readStringMap(value, "url"); url != "" {
				return []bridgeContentBlock{{Type: bridgeContentImage, URL: url}}
			}
		}
	case "image":
		if url := readStringMap(block, "url"); url != "" {
			return []bridgeContentBlock{{Type: bridgeContentImage, URL: url}}
		}
	}
	return nil
}

func decodeOpenAIToolCallBlocks(value any) []bridgeContentBlock {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	blocks := make([]bridgeContentBlock, 0, len(items))
	for _, item := range items {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		blocks = append(blocks, bridgeContentBlock{
			Type:       bridgeContentToolCall,
			ToolCallID: firstNonEmpty(readStringMap(call, "id"), readStringMap(call, "call_id")),
			ToolName:   readStringMap(function, "name"),
			Arguments:  stringifyStructuredValue(function["arguments"]),
		})
	}
	return blocks
}

func decodeOpenAIToolResultBlocks(toolCallID string, value any) []bridgeContentBlock {
	text := bridgePlainText(decodeOpenAIContentBlocks(value))
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []bridgeContentBlock{{
		Type:       bridgeContentToolResult,
		ToolCallID: strings.TrimSpace(toolCallID),
		Result:     text,
	}}
}

func decodeResponsesContent(value any) []bridgeContentBlock {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []bridgeContentBlock{{Type: bridgeContentText, Text: current}}
	case []any:
		blocks := make([]bridgeContentBlock, 0, len(current))
		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch readStringMap(block, "type") {
			case "", "text", "input_text", "output_text":
				if text := readStringMap(block, "text"); text != "" {
					blocks = append(blocks, bridgeContentBlock{Type: bridgeContentText, Text: text})
				}
			case "input_image", "image":
				if url := readStringMap(block, "image_url"); url != "" {
					blocks = append(blocks, bridgeContentBlock{Type: bridgeContentImage, URL: url})
				}
			}
		}
		return blocks
	case map[string]any:
		return decodeResponsesContent([]any{current})
	default:
		return nil
	}
}

func decodeAnthropicSystemBlocks(value any) []bridgeContentBlock {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []bridgeContentBlock{{Type: bridgeContentText, Text: strings.TrimSpace(current)}}
	case []any:
		blocks := make([]bridgeContentBlock, 0, len(current))
		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if readStringMap(block, "type") == "text" {
				if text := readStringMap(block, "text"); text != "" {
					blocks = append(blocks, bridgeContentBlock{Type: bridgeContentText, Text: text})
				}
			}
		}
		return blocks
	default:
		return nil
	}
}

func decodeAnthropicContentBlocks(value any) []bridgeContentBlock {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []bridgeContentBlock{{Type: bridgeContentText, Text: current}}
	case []any:
		blocks := make([]bridgeContentBlock, 0, len(current))
		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch readStringMap(block, "type") {
			case "text":
				if text := readStringMap(block, "text"); text != "" {
					blocks = append(blocks, bridgeContentBlock{Type: bridgeContentText, Text: text})
				}
			case "tool_use":
				blocks = append(blocks, bridgeContentBlock{
					Type:       bridgeContentToolCall,
					ToolCallID: readStringMap(block, "id"),
					ToolName:   readStringMap(block, "name"),
					Arguments:  stringifyStructuredValue(block["input"]),
				})
			case "tool_result":
				blocks = append(blocks, bridgeContentBlock{
					Type:       bridgeContentToolResult,
					ToolCallID: readStringMap(block, "tool_use_id"),
					Result:     flattenAnthropicToolResult(block["content"]),
					IsError:    readBoolMap(block, "is_error"),
				})
			case "image":
				source, _ := block["source"].(map[string]any)
				image := bridgeContentBlock{Type: bridgeContentImage}
				image.MIMEType = readStringMap(source, "media_type")
				image.Data = readStringMap(source, "data")
				if url := readStringMap(source, "url"); url != "" {
					image.URL = url
				}
				blocks = append(blocks, image)
			}
		}
		return blocks
	default:
		return nil
	}
}

func decodeGeminiParts(value any) []bridgeContentBlock {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	blocks := make([]bridgeContentBlock, 0, len(items))
	for _, item := range items {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch {
		case readStringMap(part, "text") != "":
			blocks = append(blocks, bridgeContentBlock{Type: bridgeContentText, Text: readStringMap(part, "text")})
		case part["functionCall"] != nil:
			call, _ := part["functionCall"].(map[string]any)
			blocks = append(blocks, bridgeContentBlock{
				Type:      bridgeContentToolCall,
				ToolName:  readStringMap(call, "name"),
				Arguments: stringifyStructuredValue(call["args"]),
			})
		case part["functionResponse"] != nil:
			response, _ := part["functionResponse"].(map[string]any)
			blocks = append(blocks, bridgeContentBlock{
				Type:     bridgeContentToolResult,
				ToolName: readStringMap(response, "name"),
				Result:   stringifyStructuredValue(response["response"]),
			})
		case part["inlineData"] != nil:
			inlineData, _ := part["inlineData"].(map[string]any)
			blocks = append(blocks, bridgeContentBlock{
				Type:     bridgeContentImage,
				MIMEType: readStringMap(inlineData, "mimeType"),
				Data:     readStringMap(inlineData, "data"),
			})
		case part["fileData"] != nil:
			fileData, _ := part["fileData"].(map[string]any)
			blocks = append(blocks, bridgeContentBlock{
				Type:     bridgeContentImage,
				MIMEType: readStringMap(fileData, "mimeType"),
				URL:      firstNonEmpty(readStringMap(fileData, "fileUri"), readStringMap(fileData, "uri")),
			})
		}
	}
	return blocks
}

func encodeOpenAIChatSystemMessage(blocks []bridgeContentBlock) map[string]any {
	content := encodeOpenAIContent(blocks)
	if content == nil {
		return nil
	}
	return map[string]any{
		"role":    "system",
		"content": content,
	}
}

func encodeOpenAIChatConversationMessage(message bridgeMessage) map[string]any {
	role := normalizeBridgeRole(message.Role)
	if role == "system" || role == "tool" {
		return nil
	}
	encoded := map[string]any{
		"role": role,
	}
	content := encodeOpenAIContent(message.Content)
	if content == nil {
		encoded["content"] = ""
	} else {
		encoded["content"] = content
	}
	if role == "assistant" {
		if toolCalls := encodeOpenAIToolCalls(message.Content); len(toolCalls) > 0 {
			encoded["tool_calls"] = toolCalls
		}
	}
	return encoded
}

func encodeOpenAIChatAssistantMessage(blocks []bridgeContentBlock) map[string]any {
	content := encodeOpenAIContent(blocks)
	if content == nil {
		content = ""
	}
	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if toolCalls := encodeOpenAIToolCalls(blocks); len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	return message
}

func encodeOpenAIChatToolMessages(blocks []bridgeContentBlock) []map[string]any {
	messages := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != bridgeContentToolResult {
			continue
		}
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": block.ToolCallID,
			"content":      firstNonEmpty(block.Result, block.Text),
		})
	}
	return messages
}

func encodeOpenAIContent(blocks []bridgeContentBlock) any {
	renderable := make([]bridgeContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == bridgeContentText || block.Type == bridgeContentImage {
			renderable = append(renderable, block)
		}
	}
	if len(renderable) == 0 {
		return nil
	}
	if len(renderable) == 1 && renderable[0].Type == bridgeContentText {
		return renderable[0].Text
	}
	encoded := make([]map[string]any, 0, len(renderable))
	for _, block := range renderable {
		switch block.Type {
		case bridgeContentText:
			encoded = append(encoded, map[string]any{
				"type": "text",
				"text": block.Text,
			})
		case bridgeContentImage:
			if url := encodeBridgeImageURL(block); url != "" {
				encoded = append(encoded, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": url,
					},
				})
			}
		}
	}
	if len(encoded) == 0 {
		return nil
	}
	return encoded
}

func encodeOpenAIToolCalls(blocks []bridgeContentBlock) []map[string]any {
	toolCalls := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != bridgeContentToolCall {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   fallbackID(block.ToolCallID, "call"),
			"type": "function",
			"function": map[string]any{
				"name":      block.ToolName,
				"arguments": normalizeJSONObjectString(block.Arguments),
			},
		})
	}
	return toolCalls
}

func encodeOpenAIResponsesInput(message bridgeMessage) []map[string]any {
	role := normalizeBridgeRole(message.Role)
	items := make([]map[string]any, 0, len(message.Content)+1)

	content := encodeOpenAIResponsesContent(message.Content)
	if len(content) > 0 && role != "tool" {
		items = append(items, map[string]any{
			"type":    "message",
			"role":    role,
			"content": content,
		})
	}

	for _, block := range message.Content {
		switch block.Type {
		case bridgeContentToolCall:
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   fallbackID(block.ToolCallID, "call"),
				"name":      block.ToolName,
				"arguments": normalizeJSONObjectString(block.Arguments),
			})
		case bridgeContentToolResult:
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": block.ToolCallID,
				"output":  firstNonEmpty(block.Result, block.Text),
			})
		}
	}
	return items
}

func encodeOpenAIResponsesContent(blocks []bridgeContentBlock) []map[string]any {
	content := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case bridgeContentText:
			content = append(content, map[string]any{
				"type": "input_text",
				"text": block.Text,
			})
		case bridgeContentImage:
			if url := encodeBridgeImageURL(block); url != "" {
				content = append(content, map[string]any{
					"type":      "input_image",
					"image_url": url,
				})
			}
		}
	}
	return content
}

func buildOpenAIResponsesOutput(blocks []bridgeContentBlock) []map[string]any {
	output := make([]map[string]any, 0, len(blocks)+1)

	messageContent := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case bridgeContentText:
			messageContent = append(messageContent, map[string]any{
				"type":        "output_text",
				"text":        block.Text,
				"annotations": []any{},
			})
		case bridgeContentImage:
			if url := encodeBridgeImageURL(block); url != "" {
				messageContent = append(messageContent, map[string]any{
					"type":      "input_image",
					"image_url": url,
				})
			}
		}
	}
	if len(messageContent) > 0 {
		output = append(output, map[string]any{
			"id":      model.NewID("msg"),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": messageContent,
		})
	}
	for _, block := range blocks {
		if block.Type != bridgeContentToolCall {
			continue
		}
		output = append(output, map[string]any{
			"id":        fallbackID(block.ToolCallID, "call"),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   fallbackID(block.ToolCallID, "call"),
			"name":      block.ToolName,
			"arguments": normalizeJSONObjectString(block.Arguments),
		})
	}
	return output
}

func encodeAnthropicMessage(message bridgeMessage) map[string]any {
	role := normalizeBridgeRole(message.Role)
	if role == "tool" {
		content := encodeAnthropicToolResultContent(message.Content)
		if len(content) == 0 {
			return nil
		}
		return map[string]any{
			"role":    "user",
			"content": content,
		}
	}
	content := encodeAnthropicConversationContent(message.Content, role == "assistant")
	if len(content) == 0 {
		return nil
	}
	anthropicRole := "user"
	if role == "assistant" {
		anthropicRole = "assistant"
	}
	return map[string]any{
		"role":    anthropicRole,
		"content": content,
	}
}

func encodeAnthropicAssistantContent(blocks []bridgeContentBlock) []map[string]any {
	return encodeAnthropicConversationContent(blocks, true)
}

func encodeAnthropicConversationContent(blocks []bridgeContentBlock, allowToolCalls bool) []map[string]any {
	content := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case bridgeContentText:
			content = append(content, map[string]any{
				"type": "text",
				"text": block.Text,
			})
		case bridgeContentImage:
			if source := encodeAnthropicImageSource(block); source != nil {
				content = append(content, map[string]any{
					"type":   "image",
					"source": source,
				})
			} else if block.URL != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": block.URL,
				})
			}
		case bridgeContentToolCall:
			if allowToolCalls {
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    fallbackID(block.ToolCallID, "toolu"),
					"name":  block.ToolName,
					"input": decodeJSONStringOrFallback(block.Arguments, map[string]any{}),
				})
			}
		}
	}
	return content
}

func encodeAnthropicToolResultContent(blocks []bridgeContentBlock) []map[string]any {
	content := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != bridgeContentToolResult {
			continue
		}
		content = append(content, map[string]any{
			"type":        "tool_result",
			"tool_use_id": block.ToolCallID,
			"content":     firstNonEmpty(block.Result, block.Text),
			"is_error":    block.IsError,
		})
	}
	return content
}

func encodeGeminiMessage(message bridgeMessage) map[string]any {
	role := normalizeBridgeRole(message.Role)
	parts := encodeGeminiParts(message.Content)
	if len(parts) == 0 {
		return nil
	}
	geminiRole := "user"
	if role == "assistant" {
		geminiRole = "model"
	}
	if role == "tool" {
		geminiRole = "user"
	}
	return map[string]any{
		"role":  geminiRole,
		"parts": parts,
	}
}

func encodeGeminiParts(blocks []bridgeContentBlock) []map[string]any {
	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case bridgeContentText:
			parts = append(parts, map[string]any{"text": block.Text})
		case bridgeContentToolCall:
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": block.ToolName,
					"args": decodeJSONStringOrFallback(block.Arguments, map[string]any{}),
				},
			})
		case bridgeContentToolResult:
			parts = append(parts, map[string]any{
				"functionResponse": map[string]any{
					"name": block.ToolName,
					"response": decodeJSONStringOrFallback(block.Result, map[string]any{
						"output": firstNonEmpty(block.Result, block.Text),
					}),
				},
			})
		case bridgeContentImage:
			if block.Data != "" {
				parts = append(parts, map[string]any{
					"inlineData": map[string]any{
						"mimeType": block.MIMEType,
						"data":     block.Data,
					},
				})
			} else if block.URL != "" {
				parts = append(parts, map[string]any{
					"fileData": map[string]any{
						"mimeType": block.MIMEType,
						"fileUri":  block.URL,
					},
				})
			}
		}
	}
	return parts
}

func parseOpenAIChatCompletionMessage(body map[string]any) bridgeMessage {
	choices, ok := body["choices"].([]any)
	if !ok || len(choices) == 0 {
		return bridgeMessage{Role: "assistant"}
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return bridgeMessage{Role: "assistant"}
	}
	message, _ := first["message"].(map[string]any)
	content := decodeOpenAIContentBlocks(message["content"])
	content = append(content, decodeOpenAIToolCallBlocks(message["tool_calls"])...)
	return bridgeMessage{
		Role:    "assistant",
		Content: content,
	}
}

func parseOpenAIResponsesCompletionMessage(body map[string]any) bridgeMessage {
	output, ok := body["output"].([]any)
	if !ok {
		return bridgeMessage{Role: "assistant"}
	}
	content := make([]bridgeContentBlock, 0, len(output))
	for _, item := range output {
		current, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch readStringMap(current, "type") {
		case "message":
			content = append(content, decodeResponsesContent(current["content"])...)
		case "function_call":
			content = append(content, bridgeContentBlock{
				Type:       bridgeContentToolCall,
				ToolCallID: firstNonEmpty(readStringMap(current, "call_id"), readStringMap(current, "id")),
				ToolName:   readStringMap(current, "name"),
				Arguments:  stringifyStructuredValue(current["arguments"]),
			})
		}
	}
	return bridgeMessage{
		Role:    "assistant",
		Content: content,
	}
}

func parseGeminiCompletionMessage(body map[string]any) bridgeMessage {
	candidates, ok := body["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return bridgeMessage{Role: "assistant"}
	}
	first, ok := candidates[0].(map[string]any)
	if !ok {
		return bridgeMessage{Role: "assistant"}
	}
	content, _ := first["content"].(map[string]any)
	return bridgeMessage{
		Role:    "assistant",
		Content: decodeGeminiParts(content["parts"]),
	}
}

func extractGeminiFinishReason(body map[string]any) string {
	candidates, ok := body["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return ""
	}
	first, ok := candidates[0].(map[string]any)
	if !ok {
		return ""
	}
	return readStringMap(first, "finishReason")
}

func firstChoiceFinishReasonMap(body map[string]any) string {
	choices, ok := body["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	return readStringMap(first, "finish_reason")
}

func bridgePlainText(blocks []bridgeContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == bridgeContentText && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func flattenAnthropicToolResult(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case []any:
		parts := make([]string, 0, len(current))
		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if readStringMap(block, "type") == "text" {
				if text := readStringMap(block, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return stringifyLooseValue(value)
	}
}

func encodeBridgeImageURL(block bridgeContentBlock) string {
	if block.URL != "" {
		return block.URL
	}
	if block.Data != "" {
		mimeType := firstNonEmpty(block.MIMEType, "application/octet-stream")
		return "data:" + mimeType + ";base64," + block.Data
	}
	return ""
}

func encodeAnthropicImageSource(block bridgeContentBlock) map[string]any {
	if block.Data != "" {
		return map[string]any{
			"type":       "base64",
			"media_type": firstNonEmpty(block.MIMEType, "application/octet-stream"),
			"data":       block.Data,
		}
	}
	if strings.HasPrefix(block.URL, "data:") {
		mimeType, data := parseDataURL(block.URL)
		if data != "" {
			return map[string]any{
				"type":       "base64",
				"media_type": mimeType,
				"data":       data,
			}
		}
	}
	return nil
}

func parseDataURL(value string) (string, string) {
	if !strings.HasPrefix(value, "data:") {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "data:"), ",", 2)
	if len(parts) != 2 {
		return "", ""
	}
	meta := parts[0]
	data := parts[1]
	mimeType := "application/octet-stream"
	if semi := strings.Index(meta, ";"); semi >= 0 {
		if strings.TrimSpace(meta[:semi]) != "" {
			mimeType = strings.TrimSpace(meta[:semi])
		}
	} else if strings.TrimSpace(meta) != "" {
		mimeType = strings.TrimSpace(meta)
	}
	return mimeType, data
}

func stringifyStructuredValue(value any) string {
	switch current := value.(type) {
	case nil:
		return "{}"
	case string:
		trimmed := strings.TrimSpace(current)
		if trimmed == "" {
			return "{}"
		}
		var decoded any
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			return trimmed
		}
		encoded, _ := json.Marshal(map[string]any{"value": current})
		return string(encoded)
	default:
		encoded, err := json.Marshal(current)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
}

func stringifyLooseValue(value any) string {
	switch current := value.(type) {
	case nil:
		return ""
	case string:
		return current
	default:
		encoded, err := json.Marshal(current)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func normalizeJSONObjectString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "{}"
	}
	var decoded any
	if json.Unmarshal([]byte(trimmed), &decoded) == nil {
		return trimmed
	}
	encoded, _ := json.Marshal(map[string]any{"value": value})
	return string(encoded)
}

func decodeJSONStringOrFallback(value string, fallback any) any {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	var decoded any
	if json.Unmarshal([]byte(trimmed), &decoded) == nil {
		return decoded
	}
	return fallback
}

func coalesceTextBlocks(message bridgeMessage) bridgeMessage {
	if len(message.Content) == 0 {
		return message
	}
	coalesced := make([]bridgeContentBlock, 0, len(message.Content))
	for _, block := range message.Content {
		if block.Type == bridgeContentText && len(coalesced) > 0 && coalesced[len(coalesced)-1].Type == bridgeContentText {
			coalesced[len(coalesced)-1].Text = appendParagraph(coalesced[len(coalesced)-1].Text, block.Text)
			continue
		}
		coalesced = append(coalesced, block)
	}
	message.Content = coalesced
	return message
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func readBoolMap(values map[string]any, key string) bool {
	if values == nil {
		return false
	}
	value, ok := values[key].(bool)
	return ok && value
}

func mapAnthropicStopReason(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		return "tool_use"
	case "content_filter", "safety", "refusal":
		return "refusal"
	default:
		return "end_turn"
	}
}

func mapGeminiFinishReason(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "length", "max_tokens":
		return "MAX_TOKENS"
	case "content_filter", "safety", "refusal":
		return "SAFETY"
	case "tool_use", "tool_calls", "function_call":
		return "STOP"
	default:
		return "STOP"
	}
}
