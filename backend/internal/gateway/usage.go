package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

type UsageObserver struct {
	mu        sync.Mutex
	protocol  model.Protocol
	lastEvent string
	summary   model.UsageSummary
}

func NewUsageObserver(protocol model.Protocol) *UsageObserver {
	return &UsageObserver{protocol: protocol}
}

func (o *UsageObserver) ObserveJSON(payload []byte) {
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.mergeUsageMaps(body)
}

func (o *UsageObserver) ObserveLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if bytes.HasPrefix(trimmed, []byte("event:")) {
		o.lastEvent = strings.TrimSpace(string(trimmed[len("event:"):]))
		return
	}
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}

	payload := strings.TrimSpace(string(trimmed[len("data:"):]))
	if payload == "" || payload == "[DONE]" {
		return
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return
	}
	o.mergeUsageMaps(body)
}

func (o *UsageObserver) Summary() model.UsageSummary {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.summary.TotalTokens == 0 {
		o.summary.TotalTokens = o.summary.InputTokens + o.summary.OutputTokens
	}
	return o.summary
}

func (o *UsageObserver) mergeUsageMaps(body map[string]any) {
	usageMaps := []map[string]any{}
	collectUsageMaps(body, &usageMaps)
	for _, usageMap := range usageMaps {
		o.summary.InputTokens = maxInt64(o.summary.InputTokens, readTokenInt(usageMap, "prompt_tokens"), readTokenInt(usageMap, "input_tokens"), readTokenInt(usageMap, "promptTokenCount"))
		o.summary.OutputTokens = maxInt64(o.summary.OutputTokens, readTokenInt(usageMap, "completion_tokens"), readTokenInt(usageMap, "output_tokens"), readTokenInt(usageMap, "candidatesTokenCount"))
		o.summary.TotalTokens = maxInt64(o.summary.TotalTokens, readTokenInt(usageMap, "total_tokens"), readTokenInt(usageMap, "totalTokenCount"))
		o.summary.CachedInputTokens = maxInt64(o.summary.CachedInputTokens, readTokenInt(usageMap, "cached_tokens"), readTokenInt(usageMap, "cached_input_tokens"), readTokenInt(usageMap, "cache_read_input_tokens"), readTokenInt(usageMap, "cachedContentTokenCount"))
		o.summary.ReasoningTokens = maxInt64(o.summary.ReasoningTokens, readTokenInt(usageMap, "reasoning_tokens"), readTokenInt(usageMap, "thoughtsTokenCount"))
	}
}

func collectUsageMaps(current any, out *[]map[string]any) {
	switch value := current.(type) {
	case map[string]any:
		for key, nested := range value {
			if key == "usage" || key == "usageMetadata" {
				if usageMap, ok := nested.(map[string]any); ok {
					*out = append(*out, usageMap)
				}
			}
			collectUsageMaps(nested, out)
		}
	case []any:
		for _, nested := range value {
			collectUsageMaps(nested, out)
		}
	}
}

func readTokenInt(values map[string]any, keys ...string) int64 {
	var maximum int64
	for _, key := range keys {
		if values == nil {
			continue
		}
		switch value := values[key].(type) {
		case float64:
			if int64(value) > maximum {
				maximum = int64(value)
			}
		case int:
			if int64(value) > maximum {
				maximum = int64(value)
			}
		case int64:
			if value > maximum {
				maximum = value
			}
		case json.Number:
			n, _ := value.Int64()
			if n > maximum {
				maximum = n
			}
		}
	}
	return maximum
}

func maxInt64(base int64, values ...int64) int64 {
	maximum := base
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}
