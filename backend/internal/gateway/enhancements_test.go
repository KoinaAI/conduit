package gateway

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/observability"
)

func TestAuthenticateGatewayRequestCachesInvalidSecretAndLocksSource(t *testing.T) {
	t.Parallel()

	service := &Service{
		cfg:     config.Config{GatewaySecretLookupPepper: "lookup-pepper"},
		runtime: newRuntimeState(),
	}
	state := model.RoutingState{
		GatewayKeys: []model.GatewayKey{mustGatewayKey(t, "gk-1", "valid-secret", "lookup-pepper")},
	}

	headers := http.Header{}
	headers.Set("X-API-Key", "invalid-secret")

	for attempt := 0; attempt < defaultAuthFailureLimit; attempt++ {
		_, err := service.authenticateGatewayRequest(state, headers, model.ProtocolOpenAIChat, "gpt-5.4", "203.0.113.10")
		if !errors.Is(err, errUnauthorized) {
			t.Fatalf("attempt %d: expected unauthorized, got %v", attempt+1, err)
		}
	}

	lookupHash := model.GatewaySecretLookupHash("invalid-secret", "lookup-pepper")
	if !service.runtime.invalidGatewayLookupCached(lookupHash, "", time.Now().UTC()) {
		t.Fatalf("expected invalid lookup hash %q to be cached", lookupHash)
	}

	_, err := service.authenticateGatewayRequest(state, headers, model.ProtocolOpenAIChat, "gpt-5.4", "203.0.113.10")
	if !errors.Is(err, errAuthLocked) {
		t.Fatalf("expected source to be locked after repeated invalid secrets, got %v", err)
	}
}

func TestAuthenticateGatewayRequestDoesNotPoisonValidKeyOnDisallowedAlias(t *testing.T) {
	t.Parallel()

	service := &Service{
		cfg:     config.Config{GatewaySecretLookupPepper: "lookup-pepper"},
		runtime: newRuntimeState(),
	}
	key := mustGatewayKey(t, "gk-1", "valid-secret", "lookup-pepper")
	key.AllowedModels = []string{"allowed-model"}
	state := model.RoutingState{GatewayKeys: []model.GatewayKey{key}}

	headers := http.Header{}
	headers.Set("X-API-Key", "valid-secret")

	_, err := service.authenticateGatewayRequest(state, headers, model.ProtocolOpenAIChat, "blocked-model", "198.51.100.7")
	if !errors.Is(err, errUnauthorized) {
		t.Fatalf("expected unauthorized for blocked alias, got %v", err)
	}

	authenticated, err := service.authenticateGatewayRequest(state, headers, model.ProtocolOpenAIChat, "allowed-model", "198.51.100.7")
	if err != nil {
		t.Fatalf("expected valid key to remain usable for allowed alias, got %v", err)
	}
	if authenticated.ID != "gk-1" {
		t.Fatalf("expected gateway key gk-1, got %s", authenticated.ID)
	}
	service.runtime.releaseGatewayKey(authenticated.ID, 0)
}

func TestBuildCandidatePlanWeightedRoutingPrefersHeaviestCredential(t *testing.T) {
	t.Parallel()

	service := &Service{runtime: newRuntimeState()}
	state := newRoutingModeState(model.ProviderRoutingModeWeighted)

	candidates := mustBuildCandidatePlan(t, service, state)
	if candidates[0].credential.ID != "cred-heavy" {
		t.Fatalf("expected weighted routing to prefer heavy credential, got %s", candidates[0].credential.ID)
	}
}

func TestBuildCandidatePlanLatencyRoutingPrefersFastEndpoint(t *testing.T) {
	t.Parallel()

	service := &Service{runtime: newRuntimeState()}
	state := newLatencyRoutingState()

	route := state.ModelRoutes[0]
	provider := state.Providers[0]
	target := route.Targets[0]
	credential := provider.Credentials[0]
	service.runtime.endpointState(endpointRuntimeKey(resolvedCandidate{
		provider:   provider,
		route:      route,
		target:     target,
		endpoint:   provider.Endpoints[0],
		credential: credential,
	})).LastLatencyMS = 120
	service.runtime.endpointState(endpointRuntimeKey(resolvedCandidate{
		provider:   provider,
		route:      route,
		target:     target,
		endpoint:   provider.Endpoints[1],
		credential: credential,
	})).LastLatencyMS = 15

	candidates := mustBuildCandidatePlan(t, service, state)
	if candidates[0].endpoint.ID != "endpoint-fast" {
		t.Fatalf("expected latency routing to prefer fast endpoint, got %s", candidates[0].endpoint.ID)
	}
}

func TestBuildCandidatePlanRoundRobinRotatesCandidates(t *testing.T) {
	t.Parallel()

	service := &Service{runtime: newRuntimeState()}
	state := newRoutingModeState(providerRoutingModeRoundRobin)

	first := mustBuildCandidatePlan(t, service, state)
	second := mustBuildCandidatePlan(t, service, state)

	if first[0].credential.ID == second[0].credential.ID {
		t.Fatalf("expected round-robin to rotate first candidate, got %s twice", first[0].credential.ID)
	}
}

func TestBuildCandidatePlanRouteStrategyRoundRobinRotatesProviderGroups(t *testing.T) {
	t.Parallel()

	service := &Service{runtime: newRuntimeState()}
	state := model.DefaultState()
	state.Providers = []model.Provider{
		{
			ID:             "provider-a",
			Name:           "Provider A",
			Kind:           model.ProviderKindOpenAICompatible,
			Enabled:        true,
			RoutingMode:    model.ProviderRoutingModeWeighted,
			MaxAttempts:    1,
			Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
			TimeoutSeconds: 30,
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-a",
				BaseURL: "https://provider-a.example/v1",
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{ID: "cred-a", APIKey: "key-a", Enabled: true}},
		},
		{
			ID:             "provider-b",
			Name:           "Provider B",
			Kind:           model.ProviderKindOpenAICompatible,
			Enabled:        true,
			RoutingMode:    model.ProviderRoutingModeWeighted,
			MaxAttempts:    1,
			Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
			TimeoutSeconds: 30,
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-b",
				BaseURL: "https://provider-b.example/v1",
				Enabled: true,
			}},
			Credentials: []model.ProviderCredential{{ID: "cred-b", APIKey: "key-b", Enabled: true}},
		},
	}
	state.ModelRoutes = []model.ModelRoute{{
		Alias:    "gpt-5.4",
		Strategy: model.RouteStrategyRoundRobin,
		Targets: []model.RouteTarget{
			{ID: "target-a", AccountID: "provider-a", Enabled: true, Weight: 1, MarkupMultiplier: 1},
			{ID: "target-b", AccountID: "provider-b", Enabled: true, Weight: 1, MarkupMultiplier: 1},
		},
	}}
	state.Normalize()

	first, _, _, err := service.buildCandidatePlan(state.RoutingSnapshot(), "gpt-5.4", model.ProtocolOpenAIChat, "gk-1", "")
	if err != nil {
		t.Fatalf("build first candidate plan: %v", err)
	}
	second, _, _, err := service.buildCandidatePlan(state.RoutingSnapshot(), "gpt-5.4", model.ProtocolOpenAIChat, "gk-1", "")
	if err != nil {
		t.Fatalf("build second candidate plan: %v", err)
	}
	if first[0].provider.ID == second[0].provider.ID {
		t.Fatalf("expected route strategy round-robin to rotate providers, got %s twice", first[0].provider.ID)
	}
}

func TestBuildCandidatePlanFailoverRoutingHonorsPriorityBands(t *testing.T) {
	t.Parallel()

	service := &Service{runtime: newRuntimeState()}
	state := newFailoverRoutingState()

	candidates := mustBuildCandidatePlan(t, service, state)
	if candidates[0].endpoint.ID != "endpoint-primary" {
		t.Fatalf("expected failover routing to keep primary endpoint first, got %s", candidates[0].endpoint.ID)
	}
}

func TestBuildCandidatePlanRandomRoutingKeepsUniqueCandidates(t *testing.T) {
	t.Parallel()

	service := &Service{runtime: newRuntimeState()}
	state := newRoutingModeState(providerRoutingModeRandom)

	candidates := mustBuildCandidatePlan(t, service, state)
	got := []string{candidates[0].credential.ID, candidates[1].credential.ID}
	slices.Sort(got)
	if !slices.Equal(got, []string{"cred-heavy", "cred-light"}) {
		t.Fatalf("expected random routing to keep both candidates, got %+v", got)
	}
}

func TestUsageObserverEstimatesUsageWhenUpstreamOmitsUsage(t *testing.T) {
	t.Parallel()

	observer := NewUsageObserver(model.ProtocolOpenAIChat)
	observer.ObserveRequestBody([]byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"请帮我总结一下这段内容"}]}`))
	observer.ObserveJSON([]byte(`{"id":"chatcmpl_1","choices":[{"message":{"content":"这里是一段没有 usage 的回答"}}]}`))

	summary := observer.Summary()
	if summary.InputTokens <= 0 {
		t.Fatalf("expected local fallback to estimate input tokens, got %+v", summary)
	}
	if summary.OutputTokens <= 0 {
		t.Fatalf("expected local fallback to estimate output tokens, got %+v", summary)
	}
	if summary.TotalTokens != summary.InputTokens+summary.OutputTokens {
		t.Fatalf("expected total tokens to be derived from estimates, got %+v", summary)
	}
}

func TestUsageObserverLogsMalformedSSEPayloadAndKeepsLaterUsage(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	observability.ConfigureDefaultLogger(config.Config{LogFormat: "text", LogLevel: "warn"}, &logBuffer)
	defer slog.SetDefault(originalLogger)

	observer := NewUsageObserver(model.ProtocolOpenAIResponses)
	observer.ObserveLine([]byte("event: response.completed\n"))
	observer.ObserveLine([]byte("data: {bad json}\n"))
	observer.ObserveLine([]byte("data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6}}\n"))

	summary := observer.Summary()
	if summary.TotalTokens != 10 {
		t.Fatalf("expected valid usage lines after malformed payload to still be counted, got %+v", summary)
	}
	if !strings.Contains(strings.ToLower(logBuffer.String()), "malformed sse payload") {
		t.Fatalf("expected malformed SSE payload to be logged, got %q", logBuffer.String())
	}
}

func mustGatewayKey(t *testing.T, id, secret, pepper string) model.GatewayKey {
	t.Helper()

	hash, err := model.HashGatewaySecret(secret)
	if err != nil {
		t.Fatalf("hash gateway secret: %v", err)
	}
	return model.GatewayKey{
		ID:               id,
		Name:             id,
		SecretHash:       hash,
		SecretLookupHash: model.GatewaySecretLookupHash(secret, pepper),
		Enabled:          true,
	}
}

func mustBuildCandidatePlan(t *testing.T, service *Service, state model.State) []resolvedCandidate {
	t.Helper()

	candidates, _, _, err := service.buildCandidatePlan(state.RoutingSnapshot(), "gpt-5.4", model.ProtocolOpenAIChat, "gk-1", "")
	if err != nil {
		t.Fatalf("build candidate plan: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected two candidates, got %d", len(candidates))
	}
	return candidates
}

func newRoutingModeState(mode model.ProviderRoutingMode) model.State {
	state := model.DefaultState()
	state.Providers = []model.Provider{{
		ID:             "provider-1",
		Name:           "Provider 1",
		Kind:           model.ProviderKindOpenAICompatible,
		Enabled:        true,
		RoutingMode:    mode,
		MaxAttempts:    2,
		Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
		TimeoutSeconds: 30,
		Endpoints: []model.ProviderEndpoint{{
			ID:      "endpoint-1",
			BaseURL: "https://provider-1.example/v1",
			Enabled: true,
			Weight:  1,
		}},
		Credentials: []model.ProviderCredential{
			{ID: "cred-light", APIKey: "key-light", Enabled: true, Weight: 1},
			{ID: "cred-heavy", APIKey: "key-heavy", Enabled: true, Weight: 5},
		},
	}}
	state.ModelRoutes = []model.ModelRoute{{
		Alias: "gpt-5.4",
		Targets: []model.RouteTarget{{
			ID:               "target-1",
			AccountID:        "provider-1",
			Enabled:          true,
			Weight:           1,
			MarkupMultiplier: 1,
		}},
	}}
	state.Normalize()
	return state
}

func newLatencyRoutingState() model.State {
	state := model.DefaultState()
	state.Providers = []model.Provider{{
		ID:             "provider-1",
		Name:           "Provider 1",
		Kind:           model.ProviderKindOpenAICompatible,
		Enabled:        true,
		RoutingMode:    model.ProviderRoutingModeLatency,
		MaxAttempts:    2,
		Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
		TimeoutSeconds: 30,
		Endpoints: []model.ProviderEndpoint{
			{ID: "endpoint-slow", BaseURL: "https://slow.example/v1", Enabled: true, Weight: 1},
			{ID: "endpoint-fast", BaseURL: "https://fast.example/v1", Enabled: true, Weight: 1},
		},
		Credentials: []model.ProviderCredential{
			{ID: "cred-1", APIKey: "key-1", Enabled: true, Weight: 1},
		},
	}}
	state.ModelRoutes = []model.ModelRoute{{
		Alias: "gpt-5.4",
		Targets: []model.RouteTarget{{
			ID:               "target-1",
			AccountID:        "provider-1",
			Enabled:          true,
			Weight:           1,
			MarkupMultiplier: 1,
		}},
	}}
	state.Normalize()
	return state
}

func newFailoverRoutingState() model.State {
	state := model.DefaultState()
	state.Providers = []model.Provider{{
		ID:             "provider-1",
		Name:           "Provider 1",
		Kind:           model.ProviderKindOpenAICompatible,
		Enabled:        true,
		RoutingMode:    providerRoutingModeFailover,
		MaxAttempts:    2,
		Capabilities:   []model.Protocol{model.ProtocolOpenAIChat},
		TimeoutSeconds: 30,
		Endpoints: []model.ProviderEndpoint{
			{ID: "endpoint-primary", BaseURL: "https://primary.example/v1", Enabled: true, Weight: 1, Priority: 0},
			{ID: "endpoint-secondary", BaseURL: "https://secondary.example/v1", Enabled: true, Weight: 9, Priority: 10},
		},
		Credentials: []model.ProviderCredential{
			{ID: "cred-1", APIKey: "key-1", Enabled: true, Weight: 1},
		},
	}}
	state.ModelRoutes = []model.ModelRoute{{
		Alias: "gpt-5.4",
		Targets: []model.RouteTarget{{
			ID:               "target-1",
			AccountID:        "provider-1",
			Enabled:          true,
			Weight:           1,
			MarkupMultiplier: 1,
		}},
	}}
	state.Normalize()
	return state
}
