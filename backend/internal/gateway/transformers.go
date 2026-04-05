package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func applyRequestTransformers(exchange upstreamExchange, candidate resolvedCandidate) (upstreamExchange, error) {
	transformers := filterTransformers(candidate.route.Transformers, model.TransformerPhaseRequest)
	if len(transformers) == 0 {
		return exchange, nil
	}

	contextValues := transformerContext(candidate, exchange)
	headers := http.Header{}
	removed := make([]string, 0)

	var (
		payload    map[string]any
		payloadSet bool
	)
	for _, transformer := range transformers {
		switch transformer.Type {
		case model.TransformerTypeSetHeader:
			headers.Set(transformer.Target, renderTransformerString(valueAsString(transformer.Value), contextValues))
		case model.TransformerTypeRemoveHeader:
			removed = append(removed, transformer.Target)
		case model.TransformerTypeSetJSON, model.TransformerTypeDeleteJSON, model.TransformerTypePrependMessage:
			if !payloadSet {
				nextPayload, err := decodeTransformerPayload(exchange.Body)
				if err != nil {
					return exchange, err
				}
				payload = nextPayload
				payloadSet = true
			}
			if err := applyBodyTransformer(payload, transformer, contextValues); err != nil {
				return exchange, err
			}
		}
	}

	if payloadSet {
		body, err := json.Marshal(payload)
		if err != nil {
			return exchange, err
		}
		exchange.Body = body
	}
	exchange.RequestHeaderSet = headers
	exchange.RequestHeaderRemove = removed
	return exchange, nil
}

func applyResponseTransformers(headers http.Header, payload map[string]any, transformers []model.RouteTransformer, candidate resolvedCandidate) (map[string]any, error) {
	filtered := filterTransformers(transformers, model.TransformerPhaseResponse)
	if len(filtered) == 0 {
		return payload, nil
	}

	contextValues := transformerContext(candidate, upstreamExchange{
		UpstreamModel: effectiveUpstreamModel(candidate),
		PublicAlias:   candidate.route.Alias,
	})
	for _, transformer := range filtered {
		switch transformer.Type {
		case model.TransformerTypeSetHeader:
			headers.Set(transformer.Target, renderTransformerString(valueAsString(transformer.Value), contextValues))
		case model.TransformerTypeRemoveHeader:
			headers.Del(transformer.Target)
		case model.TransformerTypeSetJSON, model.TransformerTypeDeleteJSON:
			if payload == nil {
				continue
			}
			if err := applyBodyTransformer(payload, transformer, contextValues); err != nil {
				return nil, err
			}
		}
	}
	return payload, nil
}

func applyResponseBodyTransformers(payload map[string]any, transformers []model.RouteTransformer, candidate resolvedCandidate) (map[string]any, error) {
	if payload == nil {
		return nil, nil
	}
	filtered := make([]model.RouteTransformer, 0, len(transformers))
	for _, transformer := range filterTransformers(transformers, model.TransformerPhaseResponse) {
		switch transformer.Type {
		case model.TransformerTypeSetJSON, model.TransformerTypeDeleteJSON:
			filtered = append(filtered, transformer)
		}
	}
	if len(filtered) == 0 {
		return payload, nil
	}
	return applyResponseTransformers(http.Header{}, payload, filtered, candidate)
}

func filterTransformers(transformers []model.RouteTransformer, phase model.TransformerPhase) []model.RouteTransformer {
	if len(transformers) == 0 {
		return nil
	}
	filtered := make([]model.RouteTransformer, 0, len(transformers))
	for _, transformer := range transformers {
		if transformer.Phase == phase {
			filtered = append(filtered, transformer)
		}
	}
	return filtered
}

func hasBodyTransformers(transformers []model.RouteTransformer, phase model.TransformerPhase) bool {
	for _, transformer := range transformers {
		if transformer.Phase != phase {
			continue
		}
		switch transformer.Type {
		case model.TransformerTypeSetJSON, model.TransformerTypeDeleteJSON, model.TransformerTypePrependMessage:
			return true
		}
	}
	return false
}

func decodeTransformerPayload(body []byte) (map[string]any, error) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("transformer request body must be JSON: %w", err)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, nil
}

func applyBodyTransformer(payload map[string]any, transformer model.RouteTransformer, contextValues map[string]string) error {
	switch transformer.Type {
	case model.TransformerTypeSetJSON:
		if transformer.Target == "" {
			return fmt.Errorf("transformer %q requires a target", transformer.Name)
		}
		return setJSONPath(payload, transformer.Target, renderTransformerValue(transformer.Value, contextValues))
	case model.TransformerTypeDeleteJSON:
		if transformer.Target == "" {
			return fmt.Errorf("transformer %q requires a target", transformer.Name)
		}
		deleteJSONPath(payload, transformer.Target)
		return nil
	case model.TransformerTypePrependMessage:
		return prependTransformerMessage(payload, transformer.Value, contextValues)
	default:
		return nil
	}
}

func prependTransformerMessage(payload map[string]any, raw any, contextValues map[string]string) error {
	message, ok := renderTransformerValue(raw, contextValues).(map[string]any)
	if !ok {
		return fmt.Errorf("prepend_message transformer value must be an object")
	}
	role := strings.TrimSpace(valueAsString(message["role"]))
	if role == "" {
		role = "system"
	}
	content := strings.TrimSpace(valueAsString(message["content"]))
	if content == "" {
		return fmt.Errorf("prepend_message transformer content is required")
	}
	messagePayload := map[string]any{
		"role":    role,
		"content": content,
	}
	if current, ok := payload["messages"].([]any); ok || payload["messages"] != nil || payload["input"] == nil {
		payload["messages"] = append([]any{messagePayload}, current...)
		return nil
	}
	return prependTransformerResponsesInput(payload, role, content)
}

func prependTransformerResponsesInput(payload map[string]any, role, content string) error {
	message := map[string]any{
		"role": role,
		"content": []map[string]any{{
			"type": "input_text",
			"text": content,
		}},
	}
	switch current := payload["input"].(type) {
	case nil:
		payload["input"] = []any{message}
	case string:
		payload["input"] = []any{
			message,
			map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "input_text",
					"text": current,
				}},
			},
		}
	case []any:
		payload["input"] = append([]any{message}, current...)
	default:
		return fmt.Errorf("prepend_message transformer requires messages or input to be an array or string")
	}
	return nil
}

func transformerContext(candidate resolvedCandidate, exchange upstreamExchange) map[string]string {
	return map[string]string{
		"route_alias":    strings.TrimSpace(exchange.PublicAlias),
		"upstream_model": strings.TrimSpace(exchange.UpstreamModel),
		"provider_id":    strings.TrimSpace(candidate.provider.ID),
		"provider_name":  strings.TrimSpace(candidate.provider.Name),
		"endpoint_id":    strings.TrimSpace(candidate.endpoint.ID),
		"credential_id":  strings.TrimSpace(candidate.credential.ID),
		"scenario":       strings.TrimSpace(candidate.scenario),
		"gateway_key_id": strings.TrimSpace(candidate.gatewayKeyID),
	}
}

func renderTransformerValue(value any, contextValues map[string]string) any {
	switch current := value.(type) {
	case string:
		return renderTransformerString(current, contextValues)
	case map[string]any:
		next := make(map[string]any, len(current))
		for key, item := range current {
			next[key] = renderTransformerValue(item, contextValues)
		}
		return next
	case []any:
		next := make([]any, len(current))
		for index, item := range current {
			next[index] = renderTransformerValue(item, contextValues)
		}
		return next
	default:
		return current
	}
}

func renderTransformerString(value string, contextValues map[string]string) string {
	rendered := value
	for key, replacement := range contextValues {
		rendered = strings.ReplaceAll(rendered, "${"+key+"}", replacement)
	}
	return rendered
}

func valueAsString(value any) string {
	switch current := value.(type) {
	case string:
		return current
	default:
		return fmt.Sprint(current)
	}
}

func setJSONPath(payload map[string]any, path string, value any) error {
	parts := splitTransformerPath(path)
	if len(parts) == 0 {
		return fmt.Errorf("transformer target is required")
	}
	current := payload
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func deleteJSONPath(payload map[string]any, path string) {
	parts := splitTransformerPath(path)
	if len(parts) == 0 {
		return
	}
	current := payload
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
}

func splitTransformerPath(path string) []string {
	parts := strings.Split(strings.TrimSpace(path), ".")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	return filtered
}
