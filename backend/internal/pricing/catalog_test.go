package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestFetchProfilesParsesModelsDevCatalog(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"openai": map[string]any{
				"models": map[string]any{
					"gpt-5.4": map[string]any{
						"cost": map[string]any{
							"input":      2.5,
							"output":     10.0,
							"cache_read": 0.5,
							"reasoning":  3.5,
						},
					},
					"free-model": map[string]any{},
				},
			},
			"anthropic": map[string]any{
				"models": map[string]any{
					"claude-sonnet": map[string]any{
						"cost": map[string]any{
							"input":  3.0,
							"output": 15.0,
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	service := NewService(server.URL, server.Client(), WithAllowPrivateSourceForTests())
	profiles, err := service.FetchProfiles(context.Background())
	if err != nil {
		t.Fatalf("fetch profiles: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("expected two managed pricing profiles, got %d", len(profiles))
	}
	if profiles[0].ID != "pricing-catalog-models-dev-anthropic-claude-sonnet" {
		t.Fatalf("unexpected first profile id: %q", profiles[0].ID)
	}
	if profiles[1].CachedInputPerMillion != 0.5 || profiles[1].ReasoningPerMillion != 3.5 {
		t.Fatalf("unexpected synced cost fields: %+v", profiles[1])
	}
}

func TestApplySyncResultUpsertsManagedCatalogProfiles(t *testing.T) {
	t.Parallel()

	state := model.State{
		Version: "2026-04-01",
		PricingProfiles: []model.PricingProfile{
			{ID: "manual", Name: "manual", Currency: "USD", InputPerMillion: 1},
			{ID: "pricing-catalog-models-dev-openai-gpt-5-4", Name: "old", Currency: "USD", InputPerMillion: 1},
		},
	}
	state.Normalize()

	applied := ApplySyncResult(&state, SyncResult{
		Profiles: []model.PricingProfile{
			{ID: "pricing-catalog-models-dev-openai-gpt-5-4", Name: "models.dev / openai / gpt-5.4", Currency: "USD", InputPerMillion: 2},
			{ID: "pricing-catalog-models-dev-anthropic-claude-sonnet", Name: "models.dev / anthropic / claude-sonnet", Currency: "USD", InputPerMillion: 3},
		},
	})
	if applied != 2 {
		t.Fatalf("expected two profiles to be applied, got %d", applied)
	}
	if len(state.PricingProfiles) != 3 {
		t.Fatalf("expected manual profile to be preserved, got %d profiles", len(state.PricingProfiles))
	}
	profile, ok := state.FindPricingProfile("pricing-catalog-models-dev-openai-gpt-5-4")
	if !ok || profile.InputPerMillion != 2 {
		t.Fatalf("expected managed profile to be updated, got %+v", profile)
	}
}

func TestApplySyncResultRemovesStaleManagedCatalogProfilesAndRouteBindings(t *testing.T) {
	t.Parallel()

	state := model.State{
		Version: "2026-04-01",
		PricingProfiles: []model.PricingProfile{
			{ID: "manual", Name: "manual", Currency: "USD", InputPerMillion: 1},
			{ID: "pricing-catalog-models-dev-openai-gpt-5-4", Name: "current", Currency: "USD", InputPerMillion: 2},
			{ID: "pricing-catalog-models-dev-openai-gpt-4-1", Name: "stale", Currency: "USD", InputPerMillion: 3},
		},
		ModelRoutes: []model.ModelRoute{
			{Alias: "gpt-5.4", PricingProfileID: "pricing-catalog-models-dev-openai-gpt-5-4"},
			{Alias: "gpt-4.1", PricingProfileID: "pricing-catalog-models-dev-openai-gpt-4-1"},
		},
	}
	state.Normalize()

	applied := ApplySyncResult(&state, SyncResult{
		Profiles: []model.PricingProfile{
			{ID: "pricing-catalog-models-dev-openai-gpt-5-4", Name: "models.dev / openai / gpt-5.4", Currency: "USD", InputPerMillion: 4},
		},
	})
	if applied != 1 {
		t.Fatalf("expected one managed profile to be applied, got %d", applied)
	}
	if _, ok := state.FindPricingProfile("pricing-catalog-models-dev-openai-gpt-4-1"); ok {
		t.Fatalf("expected stale managed catalog profile to be removed, got %+v", state.PricingProfiles)
	}
	if state.ModelRoutes[1].PricingProfileID != "" {
		t.Fatalf("expected route bound to stale managed profile to be cleared, got %+v", state.ModelRoutes[1])
	}
	if state.ModelRoutes[0].PricingProfileID != "pricing-catalog-models-dev-openai-gpt-5-4" {
		t.Fatalf("expected valid managed route binding to remain, got %+v", state.ModelRoutes[0])
	}
}

func TestFetchProfilesRejectsOversizedCatalogResponses(t *testing.T) {
	t.Parallel()

	oversized := `{"openai":{"models":{"gpt-5.4":{"cost":{"input":1,"output":2}},"blob":{"cost":{"input":1},"padding":"` +
		strings.Repeat("a", int(maxCatalogResponseBodyBytes)) +
		`"}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	defer server.Close()

	service := NewService(server.URL, server.Client(), WithAllowPrivateSourceForTests())
	_, err := service.FetchProfiles(context.Background())
	if err == nil {
		t.Fatalf("expected oversized catalog response to be rejected")
	}
	expected := fmt.Sprintf("pricing catalog response exceeds %d bytes", maxCatalogResponseBodyBytes)
	if !strings.Contains(err.Error(), expected) {
		t.Fatalf("expected %q, got %v", expected, err)
	}
}
