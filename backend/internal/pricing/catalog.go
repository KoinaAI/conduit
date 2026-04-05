package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const managedCatalogProfilePrefix = "pricing-catalog-models-dev-"

type Service struct {
	sourceURL string
	client    *http.Client
}

type SyncResult struct {
	Profiles   []model.PricingProfile
	SourceURL  string
	FinishedAt time.Time
	Err        error
}

type catalogProvider struct {
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	Cost map[string]float64 `json:"cost"`
}

func NewService(sourceURL string, client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Service{
		sourceURL: strings.TrimSpace(sourceURL),
		client:    client,
	}
}

func (s *Service) Enabled() bool {
	return s != nil && strings.TrimSpace(s.sourceURL) != ""
}

func (s *Service) PrepareSync(ctx context.Context) SyncResult {
	result := SyncResult{
		SourceURL:  strings.TrimSpace(s.sourceURL),
		FinishedAt: time.Now().UTC(),
	}
	if !s.Enabled() {
		result.Err = fmt.Errorf("pricing catalog source is not configured")
		return result
	}
	profiles, err := s.FetchProfiles(ctx)
	result.Profiles = profiles
	result.Err = err
	return result
}

func (s *Service) FetchProfiles(ctx context.Context) ([]model.PricingProfile, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("pricing catalog source is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pricing catalog returned status %d", resp.StatusCode)
	}

	var payload map[string]catalogProvider
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	profiles := make([]model.PricingProfile, 0, len(payload))
	for providerID, provider := range payload {
		for modelName, entry := range provider.Models {
			cost := entry.Cost
			if len(cost) == 0 {
				continue
			}
			profile := model.PricingProfile{
				ID:                    managedCatalogProfileID(providerID, modelName),
				Name:                  fmt.Sprintf("models.dev / %s / %s", strings.TrimSpace(providerID), strings.TrimSpace(modelName)),
				Currency:              "USD",
				InputPerMillion:       cost["input"],
				OutputPerMillion:      cost["output"],
				CachedInputPerMillion: cost["cache_read"],
				ReasoningPerMillion:   cost["reasoning"],
			}
			if profile.InputPerMillion == 0 &&
				profile.OutputPerMillion == 0 &&
				profile.CachedInputPerMillion == 0 &&
				profile.ReasoningPerMillion == 0 {
				continue
			}
			profiles = append(profiles, profile)
		}
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].ID < profiles[j].ID
	})
	return profiles, nil
}

func ApplySyncResult(state *model.State, result SyncResult) int {
	if state == nil || len(result.Profiles) == 0 {
		return 0
	}
	indexByID := make(map[string]int, len(state.PricingProfiles))
	for index, profile := range state.PricingProfiles {
		indexByID[profile.ID] = index
	}
	applied := 0
	for _, profile := range result.Profiles {
		if index, ok := indexByID[profile.ID]; ok {
			state.PricingProfiles[index] = profile
		} else {
			state.PricingProfiles = append(state.PricingProfiles, profile)
			indexByID[profile.ID] = len(state.PricingProfiles) - 1
		}
		applied++
	}
	sort.Slice(state.PricingProfiles, func(i, j int) bool {
		return state.PricingProfiles[i].ID < state.PricingProfiles[j].ID
	})
	return applied
}

func managedCatalogProfileID(providerID, modelName string) string {
	slug := strings.ToLower(strings.TrimSpace(providerID + "-" + modelName))
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		":", "-",
		".", "-",
		"_", "-",
	)
	slug = replacer.Replace(slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "model"
	}
	return managedCatalogProfilePrefix + slug
}
