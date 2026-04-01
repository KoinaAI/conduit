package model

import "testing"

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

	cloned := state.Clone()

	cloned.Providers[0].Capabilities[0] = ProtocolGeminiGenerate
	cloned.Providers[0].Headers["x-test"] = "b"
	cloned.ModelRoutes[0].Targets[0].Protocols[0] = ProtocolAnthropic
	cloned.Integrations[0].DefaultProtocols[0] = ProtocolGeminiStream
	cloned.Integrations[0].ModelMarkupOverrides["gpt-5"] = 2
	cloned.Integrations[0].Snapshot.ModelNames[0] = "gpt-5.4"
	cloned.Integrations[0].Snapshot.Prices["gpt-5"] = IntegrationPricing{InputPerMillion: 99}
	cloned.RequestHistory[0].ID = "req-2"

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

	snapshot := state.RoutingSnapshot()
	snapshot.Providers[0].Capabilities[0] = ProtocolGeminiGenerate
	snapshot.Providers[0].Headers["x-test"] = "b"
	snapshot.ModelRoutes[0].Targets[0].Protocols[0] = ProtocolAnthropic
	snapshot.PricingProfiles[0].Name = "Pricing 2"

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
}
