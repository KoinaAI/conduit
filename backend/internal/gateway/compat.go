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
	case model.ProtocolOpenAIChat:
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
	case model.ProtocolOpenAIResponses:
		switch providerKind {
		case model.ProviderKindOpenAICompatible:
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
		case model.ProviderKindAnthropic:
			body, stream, err := responsesToAnthropic(request.jsonPayload, upstreamModel)
			if err != nil {
				return upstreamExchange{}, err
			}
			return upstreamExchange{
				Path:          "/v1/messages",
				Body:          body,
				Stream:        stream,
				ResponseMode:  chooseResponseMode(stream, responseModeResponsesJSON, responseModeResponsesSSE),
				UpstreamModel: upstreamModel,
				PublicAlias:   request.routeAlias,
			}, nil
		case model.ProviderKindGemini:
			path, body, stream, err := responsesToGemini(request.jsonPayload, upstreamModel)
			if err != nil {
				return upstreamExchange{}, err
			}
			return upstreamExchange{
				Path:          path,
				Body:          body,
				Stream:        stream,
				ResponseMode:  chooseResponseMode(stream, responseModeResponsesJSON, responseModeResponsesSSE),
				UpstreamModel: upstreamModel,
				PublicAlias:   request.routeAlias,
			}, nil
		default:
			return upstreamExchange{}, fmt.Errorf("provider kind %s does not support protocol %s", providerKind, protocol)
		}
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

type responsesFunctionCall struct {
	ID        string
	Name      string
	Arguments string
}

type responsesEnvelope struct {
	ID        string
	Text      string
	ToolCalls []responsesFunctionCall
	Usage     model.UsageSummary
}

func responsesToAnthropic(payload map[string]any, upstreamModel string) ([]byte, bool, error) {
	chatPayload, stream, err := responsesToOpenAIChatPayload(payload, upstreamModel)
	if err != nil {
		return nil, false, err
	}
	body, err := openAIChatPayloadToAnthropic(chatPayload)
	return body, stream, err
}

func responsesToGemini(payload map[string]any, upstreamModel string) (string, []byte, bool, error) {
	chatPayload, stream, err := responsesToOpenAIChatPayload(payload, upstreamModel)
	if err != nil {
		return "", nil, false, err
	}
	body, err := openAIChatPayloadToGemini(chatPayload)
	if err != nil {
		return "", nil, false, err
	}
	path := "/v1beta/models/" + upstreamModel + ":generateContent"
	if stream {
		path = "/v1beta/models/" + upstreamModel + ":streamGenerateContent"
	}
	return path, body, stream, nil
}

func responsesToOpenAIChatPayload(payload map[string]any, upstreamModel string) (map[string]any, bool, error) {
	out := map[string]any{
		"model": upstreamModel,
	}
	stream, _ := payload["stream"].(bool)
	if stream {
		out["stream"] = true
	}
	for _, field := range []string{"temperature", "top_p"} {
		if value, ok := payload[field]; ok {
			out[field] = value
		}
	}
	if maxTokens, ok := payload["max_output_tokens"]; ok {
		out["max_tokens"] = maxTokens
	} else if maxTokens, ok := payload["max_tokens"]; ok {
		out["max_tokens"] = maxTokens
	}

	messages := make([]map[string]any, 0, 8)
	if instructions := strings.TrimSpace(stringValue(payload["instructions"])); instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	switch input := payload["input"].(type) {
	case nil:
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": input,
			})
		}
	case []any:
		items, err := responseInputItemsToOpenAIMessages(input)
		if err != nil {
			return nil, false, err
		}
		messages = append(messages, items...)
	default:
		return nil, false, errors.New("responses input must be a string or array")
	}
	out["messages"] = messages

	if tools, ok := normalizeResponsesToolDefinitions(payload["tools"]); ok {
		out["tools"] = tools
	}
	return out, stream, nil
}

func responseInputItemsToOpenAIMessages(items []any) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch current := item.(type) {
		case string:
			if strings.TrimSpace(current) == "" {
				continue
			}
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": current,
			})
		case map[string]any:
			next, err := responseInputItemToOpenAIMessages(current)
			if err != nil {
				return nil, err
			}
			messages = append(messages, next...)
		}
	}
	return messages, nil
}

func responseInputItemToOpenAIMessages(item map[string]any) ([]map[string]any, error) {
	if role := strings.TrimSpace(stringValue(item["role"])); role != "" {
		message := map[string]any{"role": role}
		content := responseMessageContentToOpenAIContent(item["content"])
		if role == "tool" {
			message["tool_call_id"] = strings.TrimSpace(firstNonEmptyString(item["tool_call_id"], item["call_id"], item["id"]))
			message["content"] = responseContentToText(item["content"])
			return []map[string]any{message}, nil
		}
		message["content"] = content
		if toolCalls := responseMessageToolCalls(item["content"]); len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		return []map[string]any{message}, nil
	}

	switch strings.TrimSpace(stringValue(item["type"])) {
	case "", "message":
		role := strings.TrimSpace(stringValue(item["role"]))
		if role == "" {
			role = "user"
		}
		return []map[string]any{{
			"role":    role,
			"content": responseMessageContentToOpenAIContent(item["content"]),
		}}, nil
	case "function_call":
		call, ok := responseFunctionCallItem(item)
		if !ok {
			return nil, nil
		}
		return []map[string]any{{
			"role":       "assistant",
			"content":    "",
			"tool_calls": []map[string]any{call},
		}}, nil
	case "function_call_output":
		toolCallID := strings.TrimSpace(firstNonEmptyString(item["call_id"], item["tool_call_id"], item["id"]))
		if toolCallID == "" {
			return nil, nil
		}
		return []map[string]any{{
			"role":         "tool",
			"tool_call_id": toolCallID,
			"content":      responseFunctionCallOutput(item["output"]),
		}}, nil
	default:
		return nil, nil
	}
}

func responseMessageContentToOpenAIContent(value any) any {
	parts := responseContentToOpenAIParts(value)
	return compactOpenAIContent(parts)
}

func responseContentToText(value any) string {
	parts := responseContentToOpenAIParts(value)
	return flattenOpenAIContent(parts)
}

func responseContentToOpenAIParts(value any) []map[string]any {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": current}}
	case []any:
		parts := make([]map[string]any, 0, len(current))
		for _, item := range current {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(stringValue(block["type"])) {
			case "", "input_text", "output_text", "text":
				text := strings.TrimSpace(firstNonEmptyString(block["text"], block["value"]))
				if text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			case "input_image", "image_url":
				if image := normalizeResponsesImagePart(block); image != nil {
					parts = append(parts, image)
				}
			}
		}
		return parts
	default:
		return nil
	}
}

func normalizeResponsesImagePart(block map[string]any) map[string]any {
	if imageURL, ok := block["image_url"].(map[string]any); ok {
		if url := strings.TrimSpace(stringValue(imageURL["url"])); url != "" {
			return map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": url,
				},
			}
		}
	}
	if imageURL := strings.TrimSpace(stringValue(block["image_url"])); imageURL != "" {
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": imageURL,
			},
		}
	}
	return nil
}

func responseMessageToolCalls(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	toolCalls := make([]map[string]any, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		call, ok := responseFunctionCallItem(block)
		if ok {
			toolCalls = append(toolCalls, call)
		}
	}
	return toolCalls
}

func responseFunctionCallItem(item map[string]any) (map[string]any, bool) {
	callID := strings.TrimSpace(firstNonEmptyString(item["call_id"], item["tool_call_id"], item["id"]))
	name := strings.TrimSpace(firstNonEmptyString(item["name"], item["tool_name"]))
	if name == "" {
		return nil, false
	}
	if callID == "" {
		callID = model.NewID("toolcall")
	}
	arguments := marshalJSONObject(item["arguments"])
	if strings.TrimSpace(arguments) == "{}" {
		if parameters, ok := item["parameters"]; ok {
			arguments = marshalJSONObject(parameters)
		}
	}
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}, true
}

func responseFunctionCallOutput(value any) string {
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return text
	}
	return marshalJSONObject(value)
}

func normalizeResponsesToolDefinitions(value any) ([]map[string]any, bool) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, false
	}
	tools := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if function, ok := tool["function"].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(stringValue(tool["type"])), "function") {
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": function,
			})
			continue
		}
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			continue
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": tool["description"],
				"parameters":  firstNonNil(tool["parameters"], tool["input_schema"]),
			},
		})
	}
	if len(tools) == 0 {
		return nil, false
	}
	return tools, true
}

func openAIChatPayloadToAnthropic(payload map[string]any) ([]byte, error) {
	out := map[string]any{
		"model": firstNonEmptyString(payload["model"]),
	}
	if maxTokens, ok := firstNonNil(payload["max_tokens"], payload["max_output_tokens"]).(float64); ok && maxTokens > 0 {
		out["max_tokens"] = int(maxTokens)
	} else if maxTokens, ok := firstNonNil(payload["max_tokens"], payload["max_output_tokens"]).(int); ok && maxTokens > 0 {
		out["max_tokens"] = maxTokens
	} else {
		out["max_tokens"] = 4096
	}
	if stream, _ := payload["stream"].(bool); stream {
		out["stream"] = true
	}
	for _, field := range []string{"temperature", "top_p"} {
		if value, ok := payload[field]; ok {
			out[field] = value
		}
	}

	systemBlocks := make([]map[string]any, 0, 2)
	messages := make([]map[string]any, 0, 8)
	if items := anySlice(payload["messages"]); len(items) > 0 {
		for _, item := range items {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role := strings.TrimSpace(stringValue(message["role"]))
			switch role {
			case "system":
				if text := flattenOpenAIContent(message["content"]); strings.TrimSpace(text) != "" {
					systemBlocks = append(systemBlocks, map[string]any{"type": "text", "text": text})
				}
			case "tool":
				if toolMessage := openAIChatToolMessageToAnthropic(message); toolMessage != nil {
					messages = append(messages, toolMessage)
				}
			default:
				if anthropicMessage := openAIChatMessageToAnthropic(message); anthropicMessage != nil {
					messages = append(messages, anthropicMessage)
				}
			}
		}
	}
	if len(systemBlocks) > 0 {
		out["system"] = systemBlocks
	}
	out["messages"] = messages
	if tools := openAIChatToolsToAnthropic(payload["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	return json.Marshal(out)
}

func openAIChatMessageToAnthropic(message map[string]any) map[string]any {
	role := strings.TrimSpace(stringValue(message["role"]))
	if role == "" {
		role = "user"
	}
	content := openAIChatContentToAnthropicBlocks(message["content"])
	if role == "assistant" {
		if toolCalls := anySlice(message["tool_calls"]); len(toolCalls) > 0 {
			for _, item := range toolCalls {
				call, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if block := openAIChatToolCallToAnthropic(call); block != nil {
					content = append(content, block)
				}
			}
		}
	}
	if len(content) == 0 {
		return nil
	}
	return map[string]any{
		"role":    role,
		"content": content,
	}
}

func openAIChatToolMessageToAnthropic(message map[string]any) map[string]any {
	toolCallID := strings.TrimSpace(firstNonEmptyString(message["tool_call_id"], message["call_id"], message["id"]))
	if toolCallID == "" {
		return openAIChatMessageToAnthropic(map[string]any{
			"role":    "user",
			"content": message["content"],
		})
	}
	content := responseContentToText(message["content"])
	if strings.TrimSpace(content) == "" {
		content = marshalJSONObject(message["content"])
	}
	return map[string]any{
		"role": "user",
		"content": []map[string]any{{
			"type":        "tool_result",
			"tool_use_id": toolCallID,
			"content": []map[string]any{{
				"type": "text",
				"text": content,
			}},
		}},
	}
}

func openAIChatContentToAnthropicBlocks(value any) []map[string]any {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": current}}
	default:
		items := anySlice(value)
		blocks := make([]map[string]any, 0, len(items))
		for _, item := range items {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(stringValue(part["type"])) {
			case "", "text":
				text := strings.TrimSpace(stringValue(part["text"]))
				if text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				if block := openAIImagePartToAnthropic(part); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		return blocks
	}
}

func openAIImagePartToAnthropic(part map[string]any) map[string]any {
	imageURL, ok := part["image_url"].(map[string]any)
	if !ok {
		return nil
	}
	rawURL := strings.TrimSpace(stringValue(imageURL["url"]))
	if rawURL == "" {
		return nil
	}
	if mediaType, data, ok := splitDataURL(rawURL); ok {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  rawURL,
		},
	}
}

func openAIChatToolCallToAnthropic(call map[string]any) map[string]any {
	function, ok := call["function"].(map[string]any)
	if !ok {
		return nil
	}
	name := strings.TrimSpace(stringValue(function["name"]))
	if name == "" {
		return nil
	}
	callID := strings.TrimSpace(stringValue(call["id"]))
	if callID == "" {
		callID = model.NewID("toolcall")
	}
	return map[string]any{
		"type":  "tool_use",
		"id":    callID,
		"name":  name,
		"input": parseJSONObjectString(stringValue(function["arguments"])),
	}
}

func openAIChatToolsToAnthropic(value any) []map[string]any {
	items := anySlice(value)
	if len(items) == 0 {
		return nil
	}
	tools := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringValue(function["name"]))
		if name == "" {
			continue
		}
		tools = append(tools, map[string]any{
			"name":         name,
			"description":  function["description"],
			"input_schema": firstNonNil(function["parameters"], map[string]any{"type": "object"}),
		})
	}
	return tools
}

func openAIChatPayloadToGemini(payload map[string]any) ([]byte, error) {
	out := map[string]any{}
	if stream, _ := payload["stream"].(bool); stream {
		// Gemini stream selection is encoded in the request path.
	}
	if generationConfig := openAIChatGenerationConfig(payload); generationConfig != nil {
		out["generationConfig"] = generationConfig
	}
	if tools := openAIChatToolsToGemini(payload["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}

	systemParts := make([]map[string]any, 0, 2)
	contents := make([]map[string]any, 0, 8)
	toolNames := map[string]string{}
	if items := anySlice(payload["messages"]); len(items) > 0 {
		for _, item := range items {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role := strings.TrimSpace(stringValue(message["role"]))
			switch role {
			case "system":
				if text := flattenOpenAIContent(message["content"]); strings.TrimSpace(text) != "" {
					systemParts = append(systemParts, map[string]any{"text": text})
				}
			case "tool":
				if content := openAIChatToolMessageToGemini(message, toolNames); content != nil {
					contents = append(contents, content)
				}
			default:
				if content := openAIChatMessageToGemini(message, toolNames); content != nil {
					contents = append(contents, content)
				}
			}
		}
	}
	if len(systemParts) > 0 {
		out["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	out["contents"] = contents
	return json.Marshal(out)
}

func openAIChatGenerationConfig(payload map[string]any) map[string]any {
	config := map[string]any{}
	if maxTokens := firstNonNil(payload["max_tokens"], payload["max_output_tokens"]); maxTokens != nil {
		config["maxOutputTokens"] = maxTokens
	}
	if temperature, ok := payload["temperature"]; ok {
		config["temperature"] = temperature
	}
	if topP, ok := payload["top_p"]; ok {
		config["topP"] = topP
	}
	if len(config) == 0 {
		return nil
	}
	return config
}

func openAIChatMessageToGemini(message map[string]any, toolNames map[string]string) map[string]any {
	role := "user"
	if strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "assistant") {
		role = "model"
	}
	parts := openAIChatContentToGeminiParts(message["content"])
	if toolCalls := anySlice(message["tool_calls"]); len(toolCalls) > 0 {
		for _, item := range toolCalls {
			call, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if part := openAIChatToolCallToGemini(call, toolNames); part != nil {
				parts = append(parts, part)
			}
		}
		role = "model"
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{
		"role":  role,
		"parts": parts,
	}
}

func openAIChatToolMessageToGemini(message map[string]any, toolNames map[string]string) map[string]any {
	callID := strings.TrimSpace(firstNonEmptyString(message["tool_call_id"], message["call_id"], message["id"]))
	if callID == "" {
		return openAIChatMessageToGemini(map[string]any{
			"role":    "user",
			"content": message["content"],
		}, toolNames)
	}
	name := strings.TrimSpace(toolNames[callID])
	if name == "" {
		name = "tool"
	}
	return map[string]any{
		"role": "function",
		"parts": []map[string]any{{
			"functionResponse": map[string]any{
				"name":     name,
				"response": parseJSONObjectString(responseFunctionCallOutput(message["content"])),
			},
		}},
	}
}

func openAIChatContentToGeminiParts(value any) []map[string]any {
	switch current := value.(type) {
	case string:
		if strings.TrimSpace(current) == "" {
			return nil
		}
		return []map[string]any{{"text": current}}
	default:
		items := anySlice(value)
		parts := make([]map[string]any, 0, len(items))
		for _, item := range items {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(stringValue(block["type"])) {
			case "", "text":
				text := strings.TrimSpace(stringValue(block["text"]))
				if text != "" {
					parts = append(parts, map[string]any{"text": text})
				}
			case "image_url":
				if image := normalizeResponsesImagePart(block); image != nil {
					parts = append(parts, map[string]any{
						"inlineData": map[string]any{
							"mimeType": detectGeminiImageMimeType(image),
							"data":     detectGeminiImageData(image),
						},
					})
				}
			}
		}
		return parts
	}
}

func openAIChatToolCallToGemini(call map[string]any, toolNames map[string]string) map[string]any {
	function, ok := call["function"].(map[string]any)
	if !ok {
		return nil
	}
	name := strings.TrimSpace(stringValue(function["name"]))
	if name == "" {
		return nil
	}
	callID := strings.TrimSpace(stringValue(call["id"]))
	if callID != "" {
		toolNames[callID] = name
	}
	return map[string]any{
		"functionCall": map[string]any{
			"name": name,
			"args": parseJSONObjectString(stringValue(function["arguments"])),
		},
	}
}

func openAIChatToolsToGemini(value any) []map[string]any {
	items := anySlice(value)
	if len(items) == 0 {
		return nil
	}
	declarations := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringValue(function["name"]))
		if name == "" {
			continue
		}
		declarations = append(declarations, map[string]any{
			"name":        name,
			"description": function["description"],
			"parameters":  firstNonNil(function["parameters"], map[string]any{"type": "object"}),
		})
	}
	if len(declarations) == 0 {
		return nil
	}
	return []map[string]any{{
		"functionDeclarations": declarations,
	}}
}

func splitDataURL(value string) (string, string, bool) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", false
	}
	mediaType, rawData, found := strings.Cut(strings.TrimPrefix(value, "data:"), ";base64,")
	if !found || strings.TrimSpace(mediaType) == "" || strings.TrimSpace(rawData) == "" {
		return "", "", false
	}
	return strings.TrimSpace(mediaType), strings.TrimSpace(rawData), true
}

func detectGeminiImageMimeType(part map[string]any) string {
	imageURL, _ := part["image_url"].(map[string]any)
	if imageURL == nil {
		return "image/png"
	}
	if mediaType, _, ok := splitDataURL(strings.TrimSpace(stringValue(imageURL["url"]))); ok {
		return mediaType
	}
	return "image/png"
}

func detectGeminiImageData(part map[string]any) string {
	imageURL, _ := part["image_url"].(map[string]any)
	if imageURL == nil {
		return ""
	}
	_, data, ok := splitDataURL(strings.TrimSpace(stringValue(imageURL["url"])))
	if !ok {
		return ""
	}
	return data
}

func parseJSONObjectString(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}
	}
	var decoded any
	if json.Unmarshal([]byte(trimmed), &decoded) == nil {
		return decoded
	}
	return map[string]any{"value": raw}
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if text := strings.TrimSpace(stringValue(value)); text != "" {
			return text
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func anySlice(value any) []any {
	switch current := value.(type) {
	case []any:
		return current
	case []map[string]any:
		out := make([]any, 0, len(current))
		for _, item := range current {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
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
	case responseModeResponsesJSON:
		return writeResponsesJSON(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeResponsesSSE:
		return writeResponsesSSE(w, resp, observer, publicAlias, transformers, candidate)
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

func writeResponsesJSON(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	payload, err := readLimitedResponseBody(resp.Body, maxTransformedResponseBodyBytes)
	if err != nil {
		return err
	}
	observer.ObserveJSON(payload)

	var envelope responsesEnvelope
	switch candidate.provider.Kind {
	case model.ProviderKindAnthropic:
		envelope, err = anthropicPayloadToResponsesEnvelope(payload, observer.Summary())
	case model.ProviderKindGemini:
		envelope, err = geminiPayloadToResponsesEnvelope(payload, observer.Summary())
	default:
		return fmt.Errorf("provider kind %s does not support responses translation", candidate.provider.Kind)
	}
	if err != nil {
		return err
	}

	body := responsesBodyFromEnvelope(envelope, publicAlias)
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

func writeResponsesSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	switch candidate.provider.Kind {
	case model.ProviderKindAnthropic:
		return writeResponsesFromAnthropicSSE(w, resp, observer, publicAlias, transformers, candidate)
	case model.ProviderKindGemini:
		return writeResponsesFromGeminiSSE(w, resp, observer, publicAlias, transformers, candidate)
	default:
		return fmt.Errorf("provider kind %s does not support responses translation", candidate.provider.Kind)
	}
}

func anthropicPayloadToResponsesEnvelope(payload []byte, usage model.UsageSummary) (responsesEnvelope, error) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return responsesEnvelope{}, err
	}
	envelope := responsesEnvelope{
		ID:    fallbackID(stringValue(body["id"]), "resp"),
		Usage: usage,
	}
	if content, ok := body["content"].([]any); ok {
		envelope.Text = flattenAnthropicTextBlocks(content)
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok || strings.TrimSpace(stringValue(block["type"])) != "tool_use" {
				continue
			}
			name := strings.TrimSpace(stringValue(block["name"]))
			if name == "" {
				continue
			}
			callID := strings.TrimSpace(firstNonEmptyString(block["id"], block["tool_use_id"]))
			if callID == "" {
				callID = model.NewID("toolcall")
			}
			envelope.ToolCalls = append(envelope.ToolCalls, responsesFunctionCall{
				ID:        callID,
				Name:      name,
				Arguments: marshalJSONObject(block["input"]),
			})
		}
	}
	return envelope, nil
}

func geminiPayloadToResponsesEnvelope(payload []byte, usage model.UsageSummary) (responsesEnvelope, error) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return responsesEnvelope{}, err
	}
	envelope := responsesEnvelope{
		ID:    fallbackID(stringValue(body["responseId"]), "resp"),
		Usage: usage,
	}
	if candidates, ok := body["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			text, calls := geminiCandidateToResponsesOutput(candidate)
			envelope.Text = text
			envelope.ToolCalls = calls
		}
	}
	return envelope, nil
}

func geminiCandidateToResponsesOutput(candidate map[string]any) (string, []responsesFunctionCall) {
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	textParts := make([]string, 0, len(parts))
	toolCalls := make([]responsesFunctionCall, 0)
	for _, item := range parts {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text := strings.TrimSpace(stringValue(part["text"])); text != "" {
			textParts = append(textParts, text)
		}
		if functionCall, ok := part["functionCall"].(map[string]any); ok {
			name := strings.TrimSpace(stringValue(functionCall["name"]))
			if name == "" {
				continue
			}
			toolCalls = append(toolCalls, responsesFunctionCall{
				ID:        model.NewID("toolcall"),
				Name:      name,
				Arguments: marshalJSONObject(functionCall["args"]),
			})
		}
	}
	return strings.Join(textParts, "\n"), toolCalls
}

func writeResponsesFromAnthropicSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
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
	lastEvent := ""
	responseID := model.NewID("resp")
	messageID := model.NewID("msg")
	createdWritten := false
	var textBuilder strings.Builder
	toolCalls := make([]responsesFunctionCall, 0)

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
			switch {
			case strings.HasPrefix(trimmed, "event:"):
				lastEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				if payload == "" || payload == "[DONE]" {
					if payload == "[DONE]" {
						err = io.EOF
						goto readNextAnthropicLine
					}
					goto readNextAnthropicLine
				}
				var body map[string]any
				if err := json.Unmarshal([]byte(payload), &body); err != nil {
					return proxyResponseWriteError{err: err}
				}
				switch lastEvent {
				case "message_start":
					if message, ok := body["message"].(map[string]any); ok {
						messageID = fallbackID(stringValue(message["id"]), "msg")
						responseID = messageID
					}
				case "content_block_start":
					if block, ok := body["content_block"].(map[string]any); ok && strings.TrimSpace(stringValue(block["type"])) == "tool_use" {
						name := strings.TrimSpace(stringValue(block["name"]))
						if name != "" {
							callID := fallbackID(stringValue(block["id"]), "toolcall")
							toolCalls = append(toolCalls, responsesFunctionCall{
								ID:        callID,
								Name:      name,
								Arguments: marshalJSONObject(block["input"]),
							})
						}
					}
				case "content_block_delta":
					if delta, ok := body["delta"].(map[string]any); ok && strings.TrimSpace(stringValue(delta["type"])) == "text_delta" {
						text := stringValue(delta["text"])
						if text != "" {
							if !createdWritten {
								if err := writeResponsesCreatedEvent(w, responseID, publicAlias, transformers, candidate); err != nil {
									return proxyResponseWriteError{err: err}
								}
								createdWritten = true
							}
							textBuilder.WriteString(text)
							if err := writeResponsesTextDeltaEvent(w, messageID, text, transformers, candidate); err != nil {
								return proxyResponseWriteError{err: err}
							}
							flush()
						}
					}
				}
				if !createdWritten {
					if err := writeResponsesCreatedEvent(w, responseID, publicAlias, transformers, candidate); err != nil {
						return proxyResponseWriteError{err: err}
					}
					createdWritten = true
				}
			}
		}
	readNextAnthropicLine:
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return proxyResponseWriteError{err: err}
		}
	}

	if !createdWritten {
		if err := writeResponsesCreatedEvent(w, responseID, publicAlias, transformers, candidate); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	if err := writeResponsesCompletedEvent(w, responsesEnvelope{
		ID:        responseID,
		Text:      textBuilder.String(),
		ToolCalls: toolCalls,
		Usage:     observer.Summary(),
	}, publicAlias, transformers, candidate); err != nil {
		return proxyResponseWriteError{err: err}
	}
	flush()
	return nil
}

func writeResponsesFromGeminiSSE(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
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
	responseID := model.NewID("resp")
	messageID := model.NewID("msg")
	createdWritten := false
	var textBuilder strings.Builder
	toolCalls := make([]responsesFunctionCall, 0)

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
				var body map[string]any
				if err := json.Unmarshal([]byte(payload), &body); err != nil {
					return proxyResponseWriteError{err: err}
				}
				if !createdWritten {
					if err := writeResponsesCreatedEvent(w, responseID, publicAlias, transformers, candidate); err != nil {
						return proxyResponseWriteError{err: err}
					}
					createdWritten = true
				}
				if candidates, ok := body["candidates"].([]any); ok && len(candidates) > 0 {
					if candidateBody, ok := candidates[0].(map[string]any); ok {
						text, calls := geminiCandidateToResponsesOutput(candidateBody)
						if text != "" {
							textBuilder.WriteString(text)
							if err := writeResponsesTextDeltaEvent(w, messageID, text, transformers, candidate); err != nil {
								return proxyResponseWriteError{err: err}
							}
							flush()
						}
						if len(calls) > 0 {
							toolCalls = append(toolCalls, calls...)
						}
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

	if !createdWritten {
		if err := writeResponsesCreatedEvent(w, responseID, publicAlias, transformers, candidate); err != nil {
			return proxyResponseWriteError{err: err}
		}
	}
	if err := writeResponsesCompletedEvent(w, responsesEnvelope{
		ID:        responseID,
		Text:      textBuilder.String(),
		ToolCalls: toolCalls,
		Usage:     observer.Summary(),
	}, publicAlias, transformers, candidate); err != nil {
		return proxyResponseWriteError{err: err}
	}
	flush()
	return nil
}

func writeResponsesCreatedEvent(w http.ResponseWriter, responseID, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	return writeResponseSSEEvent(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         fallbackID(responseID, "resp"),
			"object":     "response",
			"created_at": time.Now().UTC().Unix(),
			"status":     "in_progress",
			"model":      publicAlias,
			"output":     []any{},
		},
	}, transformers, candidate)
}

func writeResponsesTextDeltaEvent(w http.ResponseWriter, messageID, delta string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	return writeResponseSSEEvent(w, "response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       fallbackID(messageID, "msg"),
		"output_index":  0,
		"content_index": 0,
		"delta":         delta,
	}, transformers, candidate)
}

func writeResponsesCompletedEvent(w http.ResponseWriter, envelope responsesEnvelope, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	return writeResponseSSEEvent(w, "response.completed", map[string]any{
		"type":     "response.completed",
		"response": responsesBodyFromEnvelope(envelope, publicAlias),
	}, transformers, candidate)
}

func responsesBodyFromEnvelope(envelope responsesEnvelope, publicAlias string) map[string]any {
	responseID := fallbackID(envelope.ID, "resp")
	output := make([]map[string]any, 0, 1+len(envelope.ToolCalls))
	if strings.TrimSpace(envelope.Text) != "" {
		output = append(output, map[string]any{
			"id":     model.NewID("msg"),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        envelope.Text,
				"annotations": []any{},
			}},
		})
	}
	for _, call := range envelope.ToolCalls {
		output = append(output, map[string]any{
			"id":        fallbackID(call.ID, "toolcall"),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   fallbackID(call.ID, "toolcall"),
			"name":      call.Name,
			"arguments": call.Arguments,
		})
	}
	return map[string]any{
		"id":          responseID,
		"object":      "response",
		"created_at":  time.Now().UTC().Unix(),
		"status":      "completed",
		"model":       publicAlias,
		"output":      output,
		"output_text": envelope.Text,
		"usage": map[string]any{
			"input_tokens":  envelope.Usage.InputTokens,
			"output_tokens": envelope.Usage.OutputTokens,
			"total_tokens":  envelope.Usage.TotalTokens,
		},
	}
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
