package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestServiceSyncStateNewAPI(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			if r.Header.Get("Authorization") != "Bearer newapi-token" {
				t.Fatalf("unexpected auth header: %s", r.Header.Get("Authorization"))
			}
			if r.Header.Get("New-Api-User") != "42" {
				t.Fatalf("unexpected New-Api-User header: %s", r.Header.Get("New-Api-User"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota":      128,
					"used_quota": 64,
				},
			})
		case "/api/user/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{"id": "gpt-5.4"},
					{"id": "claude-3-7-sonnet"},
				},
			})
		case "/api/pricing":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{"model_name": "gpt-5.4", "input_per_million": 2.5, "output_per_million": 10.0, "currency": "USD"},
				},
			})
		case "/api/user/checkin":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"stats": map[string]any{
							"is_checked_in": false,
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_awarded": 12,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := NewService()
	state := model.DefaultState()
	state.Integrations = []model.Integration{
		{
			ID:                      "integration-1",
			Name:                    "NewAPI main",
			Kind:                    model.IntegrationKindNewAPI,
			BaseURL:                 server.URL,
			UserID:                  "42",
			AccessKey:               "newapi-token",
			RelayAPIKey:             "relay-token",
			Enabled:                 true,
			AutoCreateRoutes:        true,
			DefaultMarkupMultiplier: 1.2,
			ModelMarkupOverrides: map[string]float64{
				"gpt-5.4": 1.6,
			},
		},
	}

	snapshot, err := service.SyncState(context.Background(), &state, "integration-1")
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if snapshot.Balance != 128 || snapshot.Used != 64 {
		t.Fatalf("unexpected snapshot quotas: %+v", snapshot)
	}
	if len(snapshot.ModelNames) != 2 {
		t.Fatalf("unexpected model list: %+v", snapshot.ModelNames)
	}
	if len(state.Providers) != 1 {
		t.Fatalf("expected linked provider to be created")
	}
	if state.Providers[0].APIKey != "relay-token" {
		t.Fatalf("expected relay api key to be used for provider traffic, got %q", state.Providers[0].APIKey)
	}
	if state.Providers[0].DefaultMarkupMultiplier != 1 {
		t.Fatalf("expected integration provider multiplier to stay neutral, got %v", state.Providers[0].DefaultMarkupMultiplier)
	}
	if len(state.ModelRoutes) != 2 {
		t.Fatalf("expected routes to be auto-created")
	}
	gptRoute, ok := state.FindRoute("gpt-5.4")
	if !ok {
		t.Fatalf("expected gpt-5.4 route to be created")
	}
	if len(gptRoute.Targets) != 1 || gptRoute.Targets[0].MarkupMultiplier != 1.6 {
		t.Fatalf("expected model override multiplier to be applied once, got %+v", gptRoute.Targets)
	}
	claudeRoute, ok := state.FindRoute("claude-3-7-sonnet")
	if !ok {
		t.Fatalf("expected claude route to be created")
	}
	if len(claudeRoute.Targets) != 1 || claudeRoute.Targets[0].MarkupMultiplier != 1.2 {
		t.Fatalf("expected default multiplier on unmatched route, got %+v", claudeRoute.Targets)
	}

	if err := service.CheckinState(context.Background(), &state, "integration-1"); err != nil {
		t.Fatalf("checkin failed: %v", err)
	}
	if state.Integrations[0].Snapshot.LastCheckinResult == "" {
		t.Fatalf("expected checkin result to be recorded")
	}
}

func TestServiceSyncStateOneHub(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			if r.Header.Get("Authorization") != "Bearer onehub-token" {
				t.Fatalf("unexpected auth header: %s", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota":      256,
					"used_quota": 32,
				},
			})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "gpt-4.1"},
					{"id": "gemini-2.5-pro"},
				},
			})
		case "/api/prices":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": []map[string]any{
					{"model": "gpt-4.1", "input_per_million": 3.1, "output_per_million": 12.6, "currency": "USD"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := NewService()
	state := model.DefaultState()
	state.Integrations = []model.Integration{
		{
			ID:               "integration-2",
			Name:             "OneHub main",
			Kind:             model.IntegrationKindOneHub,
			BaseURL:          server.URL,
			AccessKey:        "onehub-token",
			Enabled:          true,
			AutoCreateRoutes: true,
		},
	}

	snapshot, err := service.SyncState(context.Background(), &state, "integration-2")
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if snapshot.Balance != 256 || snapshot.Used != 32 {
		t.Fatalf("unexpected snapshot quotas: %+v", snapshot)
	}
	if snapshot.SupportsCheckin {
		t.Fatalf("onehub should be marked as not supporting checkin by default")
	}
	if len(state.Providers) != 1 || state.Providers[0].APIKey != "onehub-token" {
		t.Fatalf("expected access key to remain the provider key when relay key is empty")
	}
	if len(state.ModelRoutes) != 2 {
		t.Fatalf("expected routes to be created for onehub models")
	}
}

func TestServiceSyncStateNewAPIFallsBackToRelayInventory(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"message": "access token invalid",
			})
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer relay-token" {
				t.Fatalf("unexpected relay auth header: %s", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "gpt-5.4"},
					{"id": "gpt-5"},
				},
			})
		case "/api/pricing":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"model_name": "gpt-5.4", "input_per_million": 2.5, "output_per_million": 10.0, "currency": "USD"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := NewService()
	state := model.DefaultState()
	state.Integrations = []model.Integration{{
		ID:               "integration-fallback",
		Name:             "Fallback NewAPI",
		Kind:             model.IntegrationKindNewAPI,
		BaseURL:          server.URL,
		UserID:           "42",
		AccessKey:        "broken-token",
		RelayAPIKey:      "relay-token",
		Enabled:          true,
		AutoCreateRoutes: true,
	}}

	snapshot, err := service.SyncState(context.Background(), &state, "integration-fallback")
	if err != nil {
		t.Fatalf("sync fallback failed: %v", err)
	}
	if snapshot.SupportsCheckin {
		t.Fatalf("relay fallback should disable checkin support")
	}
	if len(snapshot.ModelNames) != 2 {
		t.Fatalf("unexpected model list from relay fallback: %+v", snapshot.ModelNames)
	}
	if !strings.Contains(snapshot.LastError, "relay fallback inventory sync used") {
		t.Fatalf("expected fallback warning to be preserved, got %+v", snapshot)
	}
	if len(state.ModelRoutes) != 2 {
		t.Fatalf("expected routes to be created from relay fallback, got %d", len(state.ModelRoutes))
	}
}

func TestServicePrepareAndApplyDailyCheckins(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 1, 9, 0, 0, 0, time.UTC)
	var (
		mu    sync.Mutex
		calls = map[string]int{}
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		key := token + ":" + r.Method + ":" + r.URL.Path
		mu.Lock()
		calls[key]++
		mu.Unlock()

		switch token {
		case "ok-token":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"stats": map[string]any{
							"is_checked_in": false,
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_awarded": 7,
				},
			})
		case "fail-token":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"stats": map[string]any{
							"is_checked_in": false,
						},
					},
				})
				return
			}
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected token: %s", token)
		}
	}))
	defer server.Close()

	service := NewService()
	state := model.DefaultState()
	state.Integrations = []model.Integration{
		{
			ID:        "ok",
			Name:      "Checkin OK",
			Kind:      model.IntegrationKindNewAPI,
			BaseURL:   server.URL,
			AccessKey: "ok-token",
			Enabled:   true,
			Snapshot: model.IntegrationSnapshot{
				SupportsCheckin: true,
			},
		},
		{
			ID:        "fail",
			Name:      "Checkin Fail",
			Kind:      model.IntegrationKindNewAPI,
			BaseURL:   server.URL,
			AccessKey: "fail-token",
			Enabled:   true,
			Snapshot: model.IntegrationSnapshot{
				SupportsCheckin: true,
			},
		},
		{
			ID:        "fresh",
			Name:      "Already Checked",
			Kind:      model.IntegrationKindNewAPI,
			BaseURL:   server.URL,
			AccessKey: "fresh-token",
			Enabled:   true,
			Snapshot: model.IntegrationSnapshot{
				SupportsCheckin: true,
				LastCheckinAt:   &now,
			},
		},
	}

	results := service.PrepareDailyCheckins(context.Background(), state, now)
	if len(results) != 2 {
		t.Fatalf("expected two due checkins, got %d", len(results))
	}

	errs := service.ApplyDailyCheckins(&state, results)
	if len(errs) != 1 {
		t.Fatalf("expected one failing checkin, got %d", len(errs))
	}

	okIntegration, found := state.FindIntegration("ok")
	if !found {
		t.Fatalf("ok integration missing after apply")
	}
	if okIntegration.Snapshot.LastCheckinResult == "" || okIntegration.Snapshot.LastCheckinAt == nil {
		t.Fatalf("expected successful checkin metadata to be recorded: %+v", okIntegration.Snapshot)
	}
	if okIntegration.Snapshot.LastError != "" {
		t.Fatalf("did not expect error on successful checkin: %+v", okIntegration.Snapshot)
	}

	failIntegration, found := state.FindIntegration("fail")
	if !found {
		t.Fatalf("fail integration missing after apply")
	}
	if !strings.Contains(failIntegration.Snapshot.LastError, "status=500") {
		t.Fatalf("expected failing checkin error to be recorded, got %+v", failIntegration.Snapshot)
	}
	if failIntegration.Snapshot.LastCheckinResult != "checkin failed" {
		t.Fatalf("expected failing checkin result marker, got %+v", failIntegration.Snapshot)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls["fresh-token:GET:/api/user/checkin"] != 0 || calls["fresh-token:POST:/api/user/checkin"] != 0 {
		t.Fatalf("expected fresh integration to be skipped, got calls %+v", calls)
	}
}

func TestServiceCheckinStateRecognizesCheckedInToday(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/checkin":
			if r.Method != http.MethodGet {
				t.Fatalf("expected checkin probe to stop at GET, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"stats": map[string]any{
						"checked_in_today": true,
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := NewService()
	state := model.DefaultState()
	state.Integrations = []model.Integration{{
		ID:        "checked",
		Name:      "Checked Integration",
		Kind:      model.IntegrationKindNewAPI,
		BaseURL:   server.URL,
		AccessKey: "token",
		Enabled:   true,
	}}

	if err := service.CheckinState(context.Background(), &state, "checked"); err != nil {
		t.Fatalf("checkin state failed: %v", err)
	}
	integration, ok := state.FindIntegration("checked")
	if !ok {
		t.Fatalf("integration missing after checkin")
	}
	if integration.Snapshot.LastCheckinResult != "already checked in" {
		t.Fatalf("expected already checked in result, got %+v", integration.Snapshot)
	}
	if integration.Snapshot.LastCheckinAt == nil {
		t.Fatalf("expected checkin timestamp to be recorded")
	}
}
