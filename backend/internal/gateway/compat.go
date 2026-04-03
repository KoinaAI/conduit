package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

type proxyResponseWriteError struct {
	err error
}

const maxTransformedResponseBodyBytes = 8 << 20

func (e proxyResponseWriteError) Error() string {
	return e.err.Error()
}

func (e proxyResponseWriteError) Unwrap() error {
	return e.err
}

func responseWriteStarted(err error) bool {
	var writeErr proxyResponseWriteError
	return errors.As(err, &writeErr)
}

func prepareUpstreamExchange(protocol model.Protocol, providerKind model.ProviderKind, currentPath string, request parsedProxyRequest, upstreamModel string) (upstreamExchange, error) {
	switch protocol {
	case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses:
		if providerKind != model.ProviderKindOpenAICompatible {
			return upstreamExchange{}, fmt.Errorf("provider kind %s does not support protocol %s", providerKind, protocol)
		}
		payload := cloneJSONMap(request.jsonPayload)
		payload["model"] = upstreamModel
		body, err := json.Marshal(payload)
		if err != nil {
			return upstreamExchange{}, err
		}
		return upstreamExchange{
			Path:          currentPath,
			Body:          body,
			Stream:        request.stream,
			ResponseMode:  responseModePassthrough,
			UpstreamModel: upstreamModel,
			PublicAlias:   request.routeAlias,
		}, nil
	case model.ProtocolAnthropic:
		if providerKind == model.ProviderKindAnthropic {
			payload := cloneJSONMap(request.jsonPayload)
			payload["model"] = upstreamModel
			body, err := json.Marshal(payload)
			if err != nil {
				return upstreamExchange{}, err
			}
			return upstreamExchange{
				Path:          currentPath,
				Body:          body,
				Stream:        request.stream,
				ResponseMode:  responseModePassthrough,
				UpstreamModel: upstreamModel,
				PublicAlias:   request.routeAlias,
			}, nil
		}
		if providerKind == model.ProviderKindOpenAICompatible {
			body, stream, err := anthropicToOpenAIChat(request.jsonPayload, upstreamModel)
			if err != nil {
				return upstreamExchange{}, err
			}
			return upstreamExchange{
				Path:          "/v1/chat/completions",
				Body:          body,
				Stream:        stream,
				ResponseMode:  chooseResponseMode(stream, responseModeAnthropicJSON, responseModeAnthropicSSE),
				UpstreamModel: upstreamModel,
				PublicAlias:   request.routeAlias,
			}, nil
		}
	case model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		if providerKind == model.ProviderKindGemini {
			currentPath := currentPath
			parts := strings.Split(currentPath, "/models/")
			if len(parts) != 2 {
				return upstreamExchange{}, errors.New("invalid gemini path")
			}
			suffix := parts[1]
			colon := strings.Index(suffix, ":")
			if colon < 0 {
				return upstreamExchange{}, errors.New("invalid gemini model path")
			}
			rewrittenPath := parts[0] + "/models/" + upstreamModel + suffix[colon:]
			return upstreamExchange{
				Path:          rewrittenPath,
				Body:          request.rawBody,
				Stream:        protocol == model.ProtocolGeminiStream,
				ResponseMode:  responseModePassthrough,
				UpstreamModel: upstreamModel,
				PublicAlias:   request.routeAlias,
			}, nil
		}
		if providerKind == model.ProviderKindOpenAICompatible {
			body, err := geminiToOpenAIChat(request.rawBody, upstreamModel, protocol == model.ProtocolGeminiStream)
			if err != nil {
				return upstreamExchange{}, err
			}
			stream := protocol == model.ProtocolGeminiStream
			return upstreamExchange{
				Path:          "/v1/chat/completions",
				Body:          body,
				Stream:        stream,
				ResponseMode:  chooseResponseMode(stream, responseModeGeminiJSON, responseModeGeminiSSE),
				UpstreamModel: upstreamModel,
				PublicAlias:   request.routeAlias,
			}, nil
		}
	}
	return upstreamExchange{}, fmt.Errorf("provider kind %s does not support protocol %s", providerKind, protocol)
}

func rewriteProxyRequest(protocol model.Protocol, r *http.Request, request parsedProxyRequest, upstreamModel string) ([]byte, string, error) {
	providerKind := model.ProviderKindOpenAICompatible
	switch protocol {
	case model.ProtocolAnthropic:
		providerKind = model.ProviderKindAnthropic
	case model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		providerKind = model.ProviderKindGemini
	}
	exchange, err := prepareUpstreamExchange(protocol, providerKind, r.URL.Path, request, upstreamModel)
	if err != nil {
		return nil, "", err
	}
	return exchange.Body, exchange.Path, nil
}

func chooseResponseMode(stream bool, nonStream, streaming responseMode) responseMode {
	if stream {
		return streaming
	}
	return nonStream
}

func cloneJSONMap(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func anthropicToOpenAIChat(payload map[string]any, upstreamModel string) ([]byte, bool, error) {
	out := map[string]any{
		"model": upstreamModel,
	}
	if maxTokens, ok := payload["max_tokens"]; ok {
		out["max_tokens"] = maxTokens
	}
	if temperature, ok := payload["temperature"]; ok {
		out["temperature"] = temperature
	}
	if topP, ok := payload["top_p"]; ok {
		out["top_p"] = topP
	}
	stream, _ := payload["stream"].(bool)
	if stream {
		out["stream"] = true
	}

	messages := make([]map[string]any, 0, 8)
	if systemText := flattenAnthropicSystem(payload["system"]); systemText != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": systemText,
		})
	}
	if items, ok := payload["messages"].([]any); ok {
		for _, item := range items {
			current, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := current["role"].(string)
			if role == "" {
				role = "user"
			}
			messages = append(messages, map[string]any{
				"role":    role,
				"content": flattenAnthropicContent(current["content"]),
			})
		}
	}
	out["messages"] = messages

	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		mapped := make([]map[string]any, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			mapped = append(mapped, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        tool["name"],
					"description": tool["description"],
					"parameters":  tool["input_schema"],
				},
			})
		}
		if len(mapped) > 0 {
			out["tools"] = mapped
		}
	}

	body, err := json.Marshal(out)
	return body, stream, err
}

func flattenAnthropicSystem(value any) string {
	switch current := value.(type) {
	case string:
		return strings.TrimSpace(current)
	case []any:
		parts := make([]string, 0, len(current))
		for _, item := range current {
			if block, ok := item.(map[string]any); ok {
				if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func flattenAnthropicContent(value any) string {
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
			if blockType, _ := block["type"].(string); blockType != "" && blockType != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func geminiToOpenAIChat(rawBody []byte, upstreamModel string, stream bool) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return nil, fmt.Errorf("request body must be JSON: %w", err)
	}

	out := map[string]any{
		"model":  upstreamModel,
		"stream": stream,
	}

	messages := make([]map[string]any, 0, 8)
	if systemInstruction, ok := payload["systemInstruction"].(map[string]any); ok {
		if text := flattenGeminiParts(systemInstruction["parts"]); text != "" {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": text,
			})
		}
	} else if systemInstruction, ok := payload["system_instruction"].(map[string]any); ok {
		if text := flattenGeminiParts(systemInstruction["parts"]); text != "" {
			messages = append(messages, map[string]any{
				"role":    "system",
				"content": text,
			})
		}
	}
	if contents, ok := payload["contents"].([]any); ok {
		for _, item := range contents {
			content, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := content["role"].(string)
			switch role {
			case "model":
				role = "assistant"
			default:
				role = "user"
			}
			messages = append(messages, map[string]any{
				"role":    role,
				"content": flattenGeminiParts(content["parts"]),
			})
		}
	}
	out["messages"] = messages

	if generationConfig, ok := payload["generationConfig"].(map[string]any); ok {
		if maxTokens, ok := generationConfig["maxOutputTokens"]; ok {
			out["max_tokens"] = maxTokens
		}
		if temperature, ok := generationConfig["temperature"]; ok {
			out["temperature"] = temperature
		}
		if topP, ok := generationConfig["topP"]; ok {
			out["top_p"] = topP
		}
	}

	return json.Marshal(out)
}

func flattenGeminiParts(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func (s *Service) writeProxyResponse(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, exchange upstreamExchange, publicAlias string) error {
	switch exchange.ResponseMode {
	case responseModePassthrough:
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			clearWriteDeadline(w)
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if err := copyStreamingResponse(w, resp.Body, observer); err != nil {
				return proxyResponseWriteError{err: err}
			}
			return nil
		}
		payload, err := readLimitedResponseBody(resp.Body, maxTransformedResponseBodyBytes)
		if err != nil {
			return err
		}
		observer.ObserveJSON(payload)
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, err = w.Write(payload); err != nil {
			return proxyResponseWriteError{err: err}
		}
		return nil
	case responseModeAnthropicJSON:
		return writeAnthropicJSON(w, resp, observer, publicAlias)
	case responseModeAnthropicSSE:
		return writeAnthropicSSE(w, resp, observer, publicAlias)
	case responseModeGeminiJSON:
		return writeGeminiJSON(w, resp, observer, publicAlias)
	case responseModeGeminiSSE:
		return writeGeminiSSE(w, resp, observer, publicAlias)
	default:
		return fmt.Errorf("unsupported response mode %q", exchange.ResponseMode)
	}
}

func writeAnthropicJSON(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string) error {
	payload, err := readLimitedResponseBody(resp.Body, maxTransformedResponseBodyBytes)
	if err != nil {
		return err
	}
	observer.ObserveJSON(payload)
	var upstream openAIChatResponse
	if err := json.Unmarshal(payload, &upstream); err != nil {
		return err
	}
	usage := observer.Summary()
	body := map[string]any{
		"id":            fallbackID(upstream.ID, "msg"),
		"type":          "message",
		"role":          "assistant",
		"model":         publicAlias,
		"content":       []map[string]any{{"type": "text", "text": firstChoiceText(upstream)}},
		"stop_reason":   mapAnthropicStopReason(firstChoiceFinishReason(upstream)),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeAnthropicSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	reader := bufio.NewReader(resp.Body)
	flusher, _ := w.(http.Flusher)
	messageID := model.NewID("msg")
	messageStarted := false
	contentStarted := false
	finishReason := "end_turn"

	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			observer.ObserveLine(line)
			trimmed := strings.TrimSpace(string(line))
			if !strings.HasPrefix(trimmed, "data:") {
				if err == nil {
					continue
				}
				if errors.Is(err, io.EOF) {
					break
				}
				return proxyResponseWriteError{err: err}
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "" || payload == "[DONE]" {
				if payload == "[DONE]" {
					break
				}
			} else {
				var upstream openAIChatResponse
				if json.Unmarshal([]byte(payload), &upstream) == nil {
					if upstream.ID != "" {
						messageID = upstream.ID
					}
					if !messageStarted {
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
								"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
							},
						}); err != nil {
							return proxyResponseWriteError{err: err}
						}
						messageStarted = true
						flush()
					}
					deltaText := firstChoiceDeltaText(upstream)
					if deltaText != "" {
						if !contentStarted {
							if err := writeSSEEvent(w, "content_block_start", map[string]any{
								"type":          "content_block_start",
								"index":         0,
								"content_block": map[string]any{"type": "text", "text": ""},
							}); err != nil {
								return proxyResponseWriteError{err: err}
							}
							contentStarted = true
						}
						if err := writeSSEEvent(w, "content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": 0,
							"delta": map[string]any{"type": "text_delta", "text": deltaText},
						}); err != nil {
							return proxyResponseWriteError{err: err}
						}
						flush()
					}
					if finish := firstChoiceFinishReason(upstream); finish != "" {
						finishReason = mapAnthropicStopReason(finish)
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return proxyResponseWriteError{err: err}
		}
	}

	if !messageStarted {
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
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	if contentStarted {
		if err := writeSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	usage := observer.Summary()
	if err := writeSSEEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   finishReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": usage.OutputTokens,
		},
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	if err := writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	flush()
	return nil
}

func writeGeminiJSON(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string) error {
	payload, err := readLimitedResponseBody(resp.Body, maxTransformedResponseBodyBytes)
	if err != nil {
		return err
	}
	observer.ObserveJSON(payload)
	var upstream openAIChatResponse
	if err := json.Unmarshal(payload, &upstream); err != nil {
		return err
	}
	body := map[string]any{
		"candidates": []map[string]any{
			{
				"index": 0,
				"content": map[string]any{
					"role":  "model",
					"parts": []map[string]any{{"text": firstChoiceText(upstream)}},
				},
				"finishReason": mapGeminiFinishReason(firstChoiceFinishReason(upstream)),
			},
		},
		"usageMetadata": usageSummaryMap(observer.Summary()),
		"modelVersion":  publicAlias,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeGeminiSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	reader := bufio.NewReader(resp.Body)
	flusher, _ := w.(http.Flusher)
	finishReason := "STOP"

	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			observer.ObserveLine(line)
			trimmed := strings.TrimSpace(string(line))
			if !strings.HasPrefix(trimmed, "data:") {
				if err == nil {
					continue
				}
				if errors.Is(err, io.EOF) {
					break
				}
				return proxyResponseWriteError{err: err}
			}
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "" {
				// noop
			} else if payload == "[DONE]" {
				break
			} else {
				var upstream openAIChatResponse
				if json.Unmarshal([]byte(payload), &upstream) == nil {
					if finish := firstChoiceFinishReason(upstream); finish != "" {
						finishReason = mapGeminiFinishReason(finish)
					}
					deltaText := firstChoiceDeltaText(upstream)
					if deltaText != "" {
						if err := writeSSEData(w, map[string]any{
							"candidates": []map[string]any{
								{
									"index": 0,
									"content": map[string]any{
										"role":  "model",
										"parts": []map[string]any{{"text": deltaText}},
									},
								},
							},
						}); err != nil {
							return proxyResponseWriteError{err: err}
						}
						flush()
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return proxyResponseWriteError{err: err}
		}
	}

	if err := writeSSEData(w, map[string]any{
		"candidates": []map[string]any{
			{
				"index":        0,
				"content":      map[string]any{"role": "model", "parts": []map[string]any{}},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": usageSummaryMap(observer.Summary()),
		"modelVersion":  publicAlias,
	}); err != nil {
		return proxyResponseWriteError{err: err}
	}
	flush()
	return nil
}

func usageSummaryMap(summary model.UsageSummary) map[string]any {
	return map[string]any{
		"promptTokenCount":        summary.InputTokens,
		"candidatesTokenCount":    summary.OutputTokens,
		"totalTokenCount":         summary.TotalTokens,
		"cachedContentTokenCount": summary.CachedInputTokens,
	}
}

func writeSSEEvent(w http.ResponseWriter, event string, payload any) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	return writeSSEData(w, payload)
}

func writeSSEData(w http.ResponseWriter, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

func clearWriteDeadline(w http.ResponseWriter) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
}

func fallbackID(value, prefix string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return model.NewID(prefix)
}

func firstChoiceText(response openAIChatResponse) string {
	if len(response.Choices) == 0 {
		return ""
	}
	return flattenOpenAIContent(response.Choices[0].Message.Content)
}

func firstChoiceDeltaText(response openAIChatResponse) string {
	if len(response.Choices) == 0 {
		return ""
	}
	return response.Choices[0].Delta.Content
}

func firstChoiceFinishReason(response openAIChatResponse) string {
	if len(response.Choices) == 0 {
		return ""
	}
	return response.Choices[0].FinishReason
}

func flattenOpenAIContent(value any) string {
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
			if blockType, _ := block["type"].(string); blockType != "" && blockType != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func mapAnthropicStopReason(value string) string {
	switch value {
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func mapGeminiFinishReason(value string) string {
	switch value {
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return "STOP"
	}
}

func readLimitedResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, fmt.Errorf("upstream response exceeded %d bytes", maxBytes)
	}
	return payload, nil
}
