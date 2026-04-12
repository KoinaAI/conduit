package gateway

import (
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

func prepareUpstreamExchange(publicProtocol, upstreamProtocol model.Protocol, currentPath string, request parsedProxyRequest, upstreamModel string) (upstreamExchange, error) {
	if exactProtocolPassthrough(publicProtocol, upstreamProtocol) {
		switch publicProtocol {
		case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses, model.ProtocolAnthropic:
			payload := cloneJSONMap(request.jsonPayload)
			payload["model"] = upstreamModel
			body, err := json.Marshal(payload)
			if err != nil {
				return upstreamExchange{}, err
			}
			return upstreamExchange{
				Path:             currentPath,
				Body:             body,
				Stream:           request.stream,
				ResponseMode:     responseModePassthrough,
				PublicProtocol:   publicProtocol,
				UpstreamProtocol: upstreamProtocol,
				UpstreamModel:    upstreamModel,
				PublicAlias:      request.routeAlias,
			}, nil
		case model.ProtocolGeminiGenerate:
			return upstreamExchange{
				Path:             buildGeminiPath(upstreamModel, false),
				Body:             request.rawBody,
				Stream:           false,
				ResponseMode:     responseModePassthrough,
				PublicProtocol:   publicProtocol,
				UpstreamProtocol: upstreamProtocol,
				UpstreamModel:    upstreamModel,
				PublicAlias:      request.routeAlias,
			}, nil
		case model.ProtocolGeminiStream:
			return upstreamExchange{
				Path:             buildGeminiPath(upstreamModel, true),
				Body:             request.rawBody,
				Stream:           true,
				ResponseMode:     responseModePassthrough,
				PublicProtocol:   publicProtocol,
				UpstreamProtocol: upstreamProtocol,
				UpstreamModel:    upstreamModel,
				PublicAlias:      request.routeAlias,
			}, nil
		}
	}

	bridgeRequest, err := decodeBridgeRequest(publicProtocol, request)
	if err != nil {
		return upstreamExchange{}, err
	}

	body, path, err := encodeUpstreamRequest(upstreamProtocol, bridgeRequest, upstreamModel)
	if err != nil {
		return upstreamExchange{}, err
	}

	return upstreamExchange{
		Path:             path,
		Body:             body,
		Stream:           false,
		ResponseMode:     targetResponseMode(publicProtocol, request.stream),
		PublicProtocol:   publicProtocol,
		UpstreamProtocol: upstreamProtocol,
		UpstreamModel:    upstreamModel,
		PublicAlias:      request.routeAlias,
	}, nil
}

func exactProtocolPassthrough(publicProtocol, upstreamProtocol model.Protocol) bool {
	return publicProtocol == upstreamProtocol
}

func targetResponseMode(publicProtocol model.Protocol, stream bool) responseMode {
	switch publicProtocol {
	case model.ProtocolOpenAIChat:
		return chooseResponseMode(stream, responseModeOpenAIChatJSON, responseModeOpenAIChatSSE)
	case model.ProtocolOpenAIResponses:
		return chooseResponseMode(stream, responseModeOpenAIResponsesJSON, responseModeOpenAIResponsesSSE)
	case model.ProtocolAnthropic:
		return chooseResponseMode(stream, responseModeAnthropicJSON, responseModeAnthropicSSE)
	case model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		return chooseResponseMode(stream, responseModeGeminiJSON, responseModeGeminiSSE)
	default:
		return responseModePassthrough
	}
}

func rewriteProxyRequest(protocol model.Protocol, r *http.Request, request parsedProxyRequest, upstreamModel string) ([]byte, string, error) {
	exchange, err := prepareUpstreamExchange(protocol, protocol, r.URL.Path, request, upstreamModel)
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
	if exchange.ResponseMode == responseModePassthrough {
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			clearWriteDeadline(w)
			copyResponseHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if err := copyStreamingResponse(w, resp.Body, observer); err != nil {
				return proxyResponseWriteError{err: err}
			}
			return nil
		}
		payload, err := io.ReadAll(resp.Body)
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
	}
	return writeBridgeCompletion(w, resp, observer, exchange, publicAlias)
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
