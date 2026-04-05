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
			messages = append(messages, anthropicContentToOpenAIMessages(role, current["content"])...)
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
	tracker := newGeminiToolCallTracker()
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
			messages = append(messages, geminiContentToOpenAIMessages(role, content["parts"], tracker)...)
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

func anthropicContentToOpenAIMessages(role string, value any) []map[string]any {
	role = strings.TrimSpace(role)
	if role == "" {
		role = "user"
	}

	switch current := value.(type) {
	case string:
		if current == "" {
			return nil
		}
		return []map[string]any{{
			"role":    role,
			"content": current,
		}}
	case []any:
		var (
			messages  []map[string]any
			parts     []map[string]any
			toolCalls []map[string]any
		)
		flush := func(messageRole string) {
			if len(parts) == 0 && len(toolCalls) == 0 {
				return
			}
			if len(toolCalls) > 0 {
				messageRole = "assistant"
			}
			message := map[string]any{
				"role": messageRole,
			}
			if len(toolCalls) > 0 {
				message["tool_calls"] = toolCalls
				message["content"] = compactOpenAIContent(parts)
			} else {
				message["content"] = compactOpenAIContent(parts)
			}
			messages = append(messages, message)
			parts = nil
			toolCalls = nil
		}

		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(stringValue(block["type"])) {
			case "", "text":
				if text := stringValue(block["text"]); text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			case "image":
				if part, ok := anthropicImageToOpenAIContentPart(block); ok {
					parts = append(parts, part)
				}
			case "tool_use":
				if call, ok := anthropicToolUseToOpenAICall(block); ok {
					toolCalls = append(toolCalls, call)
				}
			case "tool_result":
				flush(role)
				if message, ok := anthropicToolResultToOpenAIMessage(block); ok {
					messages = append(messages, message)
				}
			}
		}
		flush(role)
		return messages
	default:
		return nil
	}
}

func anthropicImageToOpenAIContentPart(block map[string]any) (map[string]any, bool) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, false
	}
	switch strings.TrimSpace(stringValue(source["type"])) {
	case "base64":
		mediaType := strings.TrimSpace(stringValue(source["media_type"]))
		data := strings.TrimSpace(stringValue(source["data"]))
		if mediaType == "" || data == "" {
			return nil, false
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, true
	case "url":
		imageURL := strings.TrimSpace(stringValue(source["url"]))
		if imageURL == "" {
			return nil, false
		}
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": imageURL,
			},
		}, true
	default:
		return nil, false
	}
}

func anthropicToolUseToOpenAICall(block map[string]any) (map[string]any, bool) {
	name := strings.TrimSpace(stringValue(block["name"]))
	if name == "" {
		return nil, false
	}
	id := strings.TrimSpace(stringValue(block["id"]))
	if id == "" {
		id = model.NewID("toolcall")
	}
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": marshalJSONObject(block["input"]),
		},
	}, true
}

func anthropicToolResultToOpenAIMessage(block map[string]any) (map[string]any, bool) {
	content := anthropicToolResultContent(block["content"])
	toolCallID := strings.TrimSpace(stringValue(block["tool_use_id"]))
	if toolCallID == "" {
		if content == "" {
			return nil, false
		}
		return map[string]any{
			"role":    "user",
			"content": content,
		}, true
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": toolCallID,
		"content":      content,
	}, true
}

func anthropicToolResultContent(value any) string {
	if text := flattenAnthropicTextBlocks(value); text != "" {
		return text
	}
	return marshalJSONObject(value)
}

func flattenAnthropicTextBlocks(value any) string {
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
			if strings.TrimSpace(stringValue(block["type"])) != "text" {
				continue
			}
			if text := stringValue(block["text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

type geminiToolCallTracker struct {
	next          int
	pendingByName map[string][]string
}

func newGeminiToolCallTracker() *geminiToolCallTracker {
	return &geminiToolCallTracker{pendingByName: map[string][]string{}}
}

func (t *geminiToolCallTracker) newCallID(name string) string {
	t.next++
	return fmt.Sprintf("gemini-call-%d", t.next)
}

func (t *geminiToolCallTracker) remember(name, id string) {
	name = strings.TrimSpace(name)
	if name == "" || id == "" {
		return
	}
	t.pendingByName[name] = append(t.pendingByName[name], id)
}

func (t *geminiToolCallTracker) take(name string) string {
	name = strings.TrimSpace(name)
	if pending := t.pendingByName[name]; len(pending) > 0 {
		id := pending[0]
		if len(pending) == 1 {
			delete(t.pendingByName, name)
		} else {
			t.pendingByName[name] = pending[1:]
		}
		return id
	}
	return t.newCallID(name)
}

func geminiContentToOpenAIMessages(role string, value any, tracker *geminiToolCallTracker) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}

	normalizedRole := "user"
	if strings.EqualFold(strings.TrimSpace(role), "model") {
		normalizedRole = "assistant"
	}

	var (
		messages  []map[string]any
		parts     []map[string]any
		toolCalls []map[string]any
	)
	flush := func(messageRole string) {
		if len(parts) == 0 && len(toolCalls) == 0 {
			return
		}
		message := map[string]any{
			"role": messageRole,
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
			message["content"] = compactOpenAIContent(parts)
		} else {
			message["content"] = compactOpenAIContent(parts)
		}
		messages = append(messages, message)
		parts = nil
		toolCalls = nil
	}

	for _, item := range items {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text := strings.TrimSpace(stringValue(part["text"])); text != "" {
			parts = append(parts, map[string]any{"type": "text", "text": text})
		}
		if call, ok := geminiFunctionCallToOpenAICall(part, tracker); ok {
			toolCalls = append(toolCalls, call)
		}
		if message, ok := geminiFunctionResponseToOpenAIMessage(part, tracker); ok {
			flush(normalizedRole)
			messages = append(messages, message)
		}
	}

	if len(toolCalls) > 0 {
		flush("assistant")
	} else {
		flush(normalizedRole)
	}
	return messages
}

func geminiFunctionCallToOpenAICall(part map[string]any, tracker *geminiToolCallTracker) (map[string]any, bool) {
	call, ok := part["functionCall"].(map[string]any)
	if !ok {
		return nil, false
	}
	name := strings.TrimSpace(stringValue(call["name"]))
	if name == "" {
		return nil, false
	}
	id := tracker.newCallID(name)
	tracker.remember(name, id)
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": marshalJSONObject(call["args"]),
		},
	}, true
}

func geminiFunctionResponseToOpenAIMessage(part map[string]any, tracker *geminiToolCallTracker) (map[string]any, bool) {
	response, ok := part["functionResponse"].(map[string]any)
	if !ok {
		return nil, false
	}
	name := strings.TrimSpace(stringValue(response["name"]))
	if name == "" {
		return nil, false
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": tracker.take(name),
		"content":      marshalJSONObject(response["response"]),
	}, true
}

func compactOpenAIContent(parts []map[string]any) any {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 && stringValue(parts[0]["type"]) == "text" {
		return stringValue(parts[0]["text"])
	}
	return parts
}

func marshalJSONObject(value any) string {
	switch current := value.(type) {
	case nil:
		return "{}"
	case string:
		trimmed := strings.TrimSpace(current)
		if trimmed == "" {
			return "{}"
		}
		if json.Valid([]byte(trimmed)) {
			return trimmed
		}
		payload, err := json.Marshal(current)
		if err != nil {
			return "{}"
		}
		return string(payload)
	default:
		payload, err := json.Marshal(value)
		if err != nil || len(payload) == 0 {
			return "{}"
		}
		return string(payload)
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func (s *Service) writeProxyResponse(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, exchange upstreamExchange, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	switch exchange.ResponseMode {
	case responseModePassthrough:
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			clearWriteDeadline(w)
			copyResponseHeaders(w.Header(), resp.Header)
			if _, err := applyResponseTransformers(w.Header(), nil, transformers, candidate); err != nil {
				return err
			}
			w.WriteHeader(resp.StatusCode)
			if err := copyStreamingResponse(w, resp.Body, observer, transformers, candidate); err != nil {
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
		payload, err = transformPassthroughResponseBody(payload, w.Header(), transformers, candidate)
		if err != nil {
			return err
		}
		w.WriteHeader(resp.StatusCode)
		if _, err = w.Write(payload); err != nil {
			return proxyResponseWriteError{err: err}
		}
		return nil
	case responseModeAnthropicJSON:
		return writeAnthropicJSON(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeAnthropicSSE:
		return writeAnthropicSSE(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeGeminiJSON:
		return writeGeminiJSON(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeGeminiSSE:
		return writeGeminiSSE(w, resp, observer, publicAlias, transformers, candidate)
	default:
		return fmt.Errorf("unsupported response mode %q", exchange.ResponseMode)
	}
}

func transformPassthroughResponseBody(payload []byte, headers http.Header, transformers []model.RouteTransformer, candidate resolvedCandidate) ([]byte, error) {
	if len(filterTransformers(transformers, model.TransformerPhaseResponse)) == 0 {
		return payload, nil
	}
	if !hasBodyTransformers(transformers, model.TransformerPhaseResponse) {
		_, err := applyResponseTransformers(headers, nil, transformers, candidate)
		return payload, err
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	nextBody, err := applyResponseTransformers(headers, body, transformers, candidate)
	if err != nil {
		return nil, err
	}
	return json.Marshal(nextBody)
}

func transformStreamingResponseLine(line []byte, transformers []model.RouteTransformer, candidate resolvedCandidate) ([]byte, error) {
	if len(filterTransformers(transformers, model.TransformerPhaseResponse)) == 0 || !hasBodyTransformers(transformers, model.TransformerPhaseResponse) {
		return line, nil
	}
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "data:") {
		return line, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return line, nil
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return nil, err
	}
	body, err := applyResponseBodyTransformers(body, transformers, candidate)
	if err != nil {
		return nil, err
	}
	nextPayload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return []byte("data: " + string(nextPayload) + "\n"), nil
}

func writeAnthropicJSON(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
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
	body, err = applyResponseTransformers(w.Header(), body, transformers, candidate)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeAnthropicSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if _, err := applyResponseTransformers(w.Header(), nil, transformers, candidate); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)

	reader := bufio.NewReader(resp.Body)
	flusher, _ := w.(http.Flusher)
	messageID := model.NewID("msg")
	messageStarted := false
	contentStarted := false
	finishReason := "end_turn"
	sawPayload := false

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
				sawPayload = true
				var upstream openAIChatResponse
				if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
					return proxyResponseWriteError{err: err}
				}
				if upstream.ID != "" {
					messageID = upstream.ID
				}
				if !messageStarted {
					if err := writeResponseSSEEvent(w, "message_start", map[string]any{
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
					}, transformers, candidate); err != nil {
						return proxyResponseWriteError{err: err}
					}
					messageStarted = true
					flush()
				}
				deltaText := firstChoiceDeltaText(upstream)
				if deltaText != "" {
					if !contentStarted {
						if err := writeResponseSSEEvent(w, "content_block_start", map[string]any{
							"type":          "content_block_start",
							"index":         0,
							"content_block": map[string]any{"type": "text", "text": ""},
						}, transformers, candidate); err != nil {
							return proxyResponseWriteError{err: err}
						}
						contentStarted = true
					}
					if err := writeResponseSSEEvent(w, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]any{"type": "text_delta", "text": deltaText},
					}, transformers, candidate); err != nil {
						return proxyResponseWriteError{err: err}
					}
					flush()
				}
				if finish := firstChoiceFinishReason(upstream); finish != "" {
					finishReason = mapAnthropicStopReason(finish)
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
		if !sawPayload {
			return proxyResponseWriteError{err: errors.New("upstream stream ended before first response event")}
		}
	}
	if contentStarted {
		if err := writeResponseSSEEvent(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}, transformers, candidate); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	usage := observer.Summary()
	if err := writeResponseSSEEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   finishReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": usage.OutputTokens,
		},
	}, transformers, candidate); err != nil {
		return proxyResponseWriteError{err: err}
	}
	if err := writeResponseSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"}, transformers, candidate); err != nil {
		return proxyResponseWriteError{err: err}
	}
	flush()
	return nil
}

func writeGeminiJSON(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
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
	body, err = applyResponseTransformers(w.Header(), body, transformers, candidate)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return proxyResponseWriteError{err: err}
	}
	return nil
}

func writeGeminiSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	clearWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if _, err := applyResponseTransformers(w.Header(), nil, transformers, candidate); err != nil {
		return err
	}
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
				if err := json.Unmarshal([]byte(payload), &upstream); err != nil {
					return proxyResponseWriteError{err: err}
				}
				if finish := firstChoiceFinishReason(upstream); finish != "" {
					finishReason = mapGeminiFinishReason(finish)
				}
				deltaText := firstChoiceDeltaText(upstream)
				if deltaText != "" {
					if err := writeResponseSSEData(w, map[string]any{
						"candidates": []map[string]any{
							{
								"index": 0,
								"content": map[string]any{
									"role":  "model",
									"parts": []map[string]any{{"text": deltaText}},
								},
							},
						},
					}, transformers, candidate); err != nil {
						return proxyResponseWriteError{err: err}
					}
					flush()
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

	if err := writeResponseSSEData(w, map[string]any{
		"candidates": []map[string]any{
			{
				"index":        0,
				"finishReason": finishReason,
			},
		},
		"usageMetadata": usageSummaryMap(observer.Summary()),
		"modelVersion":  publicAlias,
	}, transformers, candidate); err != nil {
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

func writeResponseSSEEvent(w http.ResponseWriter, event string, payload map[string]any, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	return writeResponseSSEData(w, payload, transformers, candidate)
}

func writeResponseSSEData(w http.ResponseWriter, payload map[string]any, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	nextPayload, err := applyResponseBodyTransformers(payload, transformers, candidate)
	if err != nil {
		return err
	}
	if nextPayload == nil {
		nextPayload = payload
	}
	return writeSSEData(w, nextPayload)
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
		return "refusal"
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
