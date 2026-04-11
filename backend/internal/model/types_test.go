package model

import (
	"slices"
	"testing"
	"time"
)

func TestStateCloneDeepCopiesNestedCollections(t *testing.T) {
	t.Parallel()

	state := DefaultState()
	state.Providers = []Provider{
		{
			ID:           "provider-1",
			Name:         "Provider 1",
			Capabilities: []Protocol{ProtocolOpenAIChat},
			Headers:      map[string]string{"x-test": "a"},
		},
	}
	state.ModelRoutes = []ModelRoute{
		{
			Alias: "gpt-5",
			Targets: []RouteTarget{
				{
					ID:        "target-1",
					AccountID: "provider-1",
					Protocols: []Protocol{ProtocolOpenAIResponses},
				},
			},
		},
	}
	state.Integrations = []Integration{
		{
			ID:                   "integration-1",
			DefaultProtocols:     []Protocol{ProtocolOpenAIChat},
			ModelMarkupOverrides: map[string]float64{"gpt-5": 1.5},
			Snapshot: IntegrationSnapshot{
				ModelNames: []string{"gpt-5"},
				Prices: map[string]IntegrationPricing{
					"gpt-5": {InputPerMillion: 1},
				},
			},
		},
	}
	state.RequestHistory = []RequestRecord{{ID: "req-1"}}
	expiresAt := time.Date(2026, time.April, 3, 10, 0, 0, 0, time.UTC)
	lastUsedAt := time.Date(2026, time.April, 3, 11, 0, 0, 0, time.UTC)
	state.GatewayKeys = []GatewayKey{{
		ID:               "gk-1",
		Name:             "gateway",
		SecretHash:       "hash",
		SecretLookupHash: "lookup",
		SecretPreview:    "preview",
		Enabled:          true,
		ExpiresAt:        &expiresAt,
		LastUsedAt:       &lastUsedAt,
		AllowedModels:    []string{"gpt-5"},
		AllowedProtocols: []Protocol{ProtocolOpenAIChat},
	}}

	cloned := state.Clone()

	cloned.Providers[0].Capabilities[0] = ProtocolGeminiGenerate
	cloned.Providers[0].Headers["x-test"] = "b"
	cloned.ModelRoutes[0].Targets[0].Protocols[0] = ProtocolAnthropic
	cloned.Integrations[0].DefaultProtocols[0] = ProtocolGeminiStream
	cloned.Integrations[0].ModelMarkupOverrides["gpt-5"] = 2
	cloned.Integrations[0].Snapshot.ModelNames[0] = "gpt-5.4"
	cloned.Integrations[0].Snapshot.Prices["gpt-5"] = IntegrationPricing{InputPerMillion: 99}
	cloned.RequestHistory[0].ID = "req-2"
	cloned.GatewayKeys[0].AllowedModels[0] = "gpt-5.4"
	cloned.GatewayKeys[0].AllowedProtocols[0] = ProtocolAnthropic
	*cloned.GatewayKeys[0].ExpiresAt = expiresAt.Add(time.Hour)
	*cloned.GatewayKeys[0].LastUsedAt = lastUsedAt.Add(time.Hour)

	if state.Providers[0].Capabilities[0] != ProtocolOpenAIChat {
		t.Fatalf("provider capabilities were not cloned")
	}
	if state.Providers[0].Headers["x-test"] != "a" {
		t.Fatalf("provider headers were not cloned")
	}
	if state.ModelRoutes[0].Targets[0].Protocols[0] != ProtocolOpenAIResponses {
		t.Fatalf("route target protocols were not cloned")
	}
	if state.Integrations[0].DefaultProtocols[0] != ProtocolOpenAIChat {
		t.Fatalf("integration protocols were not cloned")
	}
	if state.Integrations[0].ModelMarkupOverrides["gpt-5"] != 1.5 {
		t.Fatalf("integration overrides were not cloned")
	}
	if state.Integrations[0].Snapshot.ModelNames[0] != "gpt-5" {
		t.Fatalf("integration model names were not cloned")
	}
	if state.Integrations[0].Snapshot.Prices["gpt-5"].InputPerMillion != 1 {
		t.Fatalf("integration prices were not cloned")
	}
	if state.RequestHistory[0].ID != "req-1" {
		t.Fatalf("request history values were unexpectedly mutated")
	}
	if state.GatewayKeys[0].AllowedModels[0] != "gpt-5" {
		t.Fatalf("gateway key allowed models were not cloned")
	}
	if state.GatewayKeys[0].AllowedProtocols[0] != ProtocolOpenAIChat {
		t.Fatalf("gateway key allowed protocols were not cloned")
	}
	if !state.GatewayKeys[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("gateway key expires_at pointer was not cloned")
	}
	if !state.GatewayKeys[0].LastUsedAt.Equal(lastUsedAt) {
		t.Fatalf("gateway key last_used_at pointer was not cloned")
	}
}

func TestStateRoutingSnapshotDeepCopiesRoutingCollections(t *testing.T) {
	t.Parallel()

	state := DefaultState()
	state.Providers = []Provider{
		{
			ID:           "provider-1",
			Name:         "Provider 1",
			Capabilities: []Protocol{ProtocolOpenAIChat},
			Headers:      map[string]string{"x-test": "a"},
		},
	}
	state.ModelRoutes = []ModelRoute{
		{
			Alias: "gpt-5",
			Targets: []RouteTarget{
				{
					ID:        "target-1",
					AccountID: "provider-1",
					Protocols: []Protocol{ProtocolOpenAIResponses},
				},
			},
		},
	}
	state.PricingProfiles = []PricingProfile{{ID: "pricing-1", Name: "Pricing 1"}}
	expiresAt := time.Date(2026, time.April, 3, 10, 0, 0, 0, time.UTC)
	state.GatewayKeys = []GatewayKey{{
		ID:               "gk-1",
		AllowedModels:    []string{"gpt-5"},
		AllowedProtocols: []Protocol{ProtocolOpenAIChat},
		ExpiresAt:        &expiresAt,
	}}

	snapshot := state.RoutingSnapshot()
	snapshot.Providers[0].Capabilities[0] = ProtocolGeminiGenerate
	snapshot.Providers[0].Headers["x-test"] = "b"
	snapshot.ModelRoutes[0].Targets[0].Protocols[0] = ProtocolAnthropic
	snapshot.PricingProfiles[0].Name = "Pricing 2"
	snapshot.GatewayKeys[0].AllowedModels[0] = "gpt-5.4"
	snapshot.GatewayKeys[0].AllowedProtocols[0] = ProtocolAnthropic
	*snapshot.GatewayKeys[0].ExpiresAt = expiresAt.Add(time.Hour)

	if state.Providers[0].Capabilities[0] != ProtocolOpenAIChat {
		t.Fatalf("provider capabilities were not cloned")
	}
	if state.Providers[0].Headers["x-test"] != "a" {
		t.Fatalf("provider headers were not cloned")
	}
	if state.ModelRoutes[0].Targets[0].Protocols[0] != ProtocolOpenAIResponses {
		t.Fatalf("route target protocols were not cloned")
	}
	if state.PricingProfiles[0].Name != "Pricing 1" {
		t.Fatalf("pricing profiles were not cloned")
	}
	if state.GatewayKeys[0].AllowedModels[0] != "gpt-5" {
		t.Fatalf("gateway key allowed models were not cloned")
	}
	if state.GatewayKeys[0].AllowedProtocols[0] != ProtocolOpenAIChat {
		t.Fatalf("gateway key allowed protocols were not cloned")
	}
	if !state.GatewayKeys[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("gateway key expires_at pointer was not cloned")
	}
}

func TestStateNormalizeAssignsProviderCapabilitiesByKind(t *testing.T) {
	t.Parallel()

	state := DefaultState()
	state.Providers = []Provider{
		{ID: "openai", Kind: ProviderKindOpenAICompatible},
		{ID: "anthropic", Kind: ProviderKindAnthropic},
		{ID: "gemini", Kind: ProviderKindGemini},
	}

	state.Normalize()

	if got := state.Providers[0].Capabilities; !slices.Equal(got, []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses}) {
		t.Fatalf("unexpected openai-compatible capabilities: %+v", got)
	}
	if got := state.Providers[1].Capabilities; !slices.Equal(got, []Protocol{ProtocolAnthropic, ProtocolOpenAIChat, ProtocolOpenAIResponses}) {
		t.Fatalf("unexpected anthropic capabilities: %+v", got)
	}
	if got := state.Providers[2].Capabilities; !slices.Equal(got, []Protocol{ProtocolGeminiGenerate, ProtocolGeminiStream, ProtocolOpenAIChat, ProtocolOpenAIResponses}) {
		t.Fatalf("unexpected gemini capabilities: %+v", got)
	}
}

func TestStateCloneDoesNotNormalizeMissingIdentifiers(t *testing.T) {
	t.Parallel()

	state := State{
		Providers: []Provider{{
			Name:        "provider-without-id",
			Kind:        ProviderKindOpenAICompatible,
			Enabled:     true,
			Endpoints:   []ProviderEndpoint{{BaseURL: "https://provider.example/v1", Enabled: true}},
			Credentials: []ProviderCredential{{APIKey: "provider-key", Enabled: true}},
		}},
	}

	cloned := state.Clone()

	if cloned.Providers[0].ID != "" {
		t.Fatalf("expected clone to preserve missing provider id, got %q", cloned.Providers[0].ID)
	}
	if cloned.Providers[0].Endpoints[0].ID != "" {
		t.Fatalf("expected clone to preserve missing endpoint id, got %q", cloned.Providers[0].Endpoints[0].ID)
	}
	if cloned.Providers[0].Credentials[0].ID != "" {
		t.Fatalf("expected clone to preserve missing credential id, got %q", cloned.Providers[0].Credentials[0].ID)
	}
}

func TestResolvePricingProfileFallsBackToAliasRules(t *testing.T) {
	t.Parallel()

	state := DefaultState()
	state.PricingProfiles = []PricingProfile{
		{ID: "standard", Name: "Standard"},
		{ID: "catalog-gpt-5", Name: "Catalog GPT-5"},
	}
	state.PricingAliases = []PricingAliasRule{
		{
			Name:             "prefix-match",
			MatchType:        PricingAliasMatchPrefix,
			Pattern:          "gpt-5",
			PricingProfileID: "catalog-gpt-5",
			Enabled:          true,
		},
	}

	profile, ok := state.ResolvePricingProfile("", "gpt-5.4-fast", "openai/gpt-5.4-fast")
	if !ok {
		t.Fatal("expected pricing alias rule to resolve a profile")
	}
	if profile.ID != "catalog-gpt-5" {
		t.Fatalf("expected alias rule to resolve catalog-gpt-5, got %+v", profile)
	}

	explicit, ok := state.ResolvePricingProfile("standard", "gpt-5.4-fast", "openai/gpt-5.4-fast")
	if !ok || explicit.ID != "standard" {
		t.Fatalf("expected explicit route pricing profile to win, got %+v ok=%v", explicit, ok)
	}
}
