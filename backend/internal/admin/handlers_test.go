package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/integration"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

func TestRunCheckinsSkipsAlreadyCheckedInIntegrations(t *testing.T) {
	t.Parallel()

	var getCount atomic.Int64
	var postCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/checkin":
			getCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"stats": map[string]any{
						"is_checked_in": false,
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/user/checkin":
			postCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_awarded": 8,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Integrations: []model.Integration{
			{
				ID:        "integration-1",
				Name:      "NewAPI",
				Kind:      model.IntegrationKindNewAPI,
				BaseURL:   server.URL,
				UserID:    "132",
				AccessKey: "token",
				Enabled:   true,
				Snapshot: model.IntegrationSnapshot{
					SupportsCheckin: true,
				},
			},
		},
	})

	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))
	handlers.RunCheckins(context.Background())
	handlers.RunCheckins(context.Background())

	if got := getCount.Load(); got != 1 {
		t.Fatalf("expected one GET status probe, got %d", got)
	}
	if got := postCount.Load(); got != 1 {
		t.Fatalf("expected one POST checkin, got %d", got)
	}

	saved := fileStore.Snapshot()
	lastCheckin := saved.Integrations[0].Snapshot.LastCheckinAt
	if lastCheckin == nil {
		t.Fatalf("expected last_checkin_at to be recorded")
	}
	if saved.Integrations[0].Snapshot.LastError != "" {
		t.Fatalf("expected no persisted error, got %q", saved.Integrations[0].Snapshot.LastError)
	}
}

func TestGetStateRedactsSensitiveFields(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Providers: []model.Provider{{
			ID:      "provider-1",
			Name:    "Provider",
			Kind:    model.ProviderKindOpenAICompatible,
			BaseURL: "https://provider.example",
			APIKey:  "provider-secret-key",
			Enabled: true,
			Credentials: []model.ProviderCredential{{
				ID:      "cred-1",
				APIKey:  "credential-secret-key",
				Enabled: true,
			}},
		}},
		Integrations: []model.Integration{{
			ID:          "integration-1",
			Name:        "NewAPI",
			Kind:        model.IntegrationKindNewAPI,
			BaseURL:     "https://relay.example",
			AccessKey:   "access-secret-key",
			RelayAPIKey: "relay-secret-key",
			Enabled:     true,
		}},
		GatewayKeys: []model.GatewayKey{{
			ID:               "gk-1",
			Name:             "gateway",
			SecretLookupHash: "lookup-hash",
			SecretPreview:    "uag-12...abcd",
			Enabled:          true,
		}},
	})

	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))
	req := httptest.NewRequest(http.MethodGet, "/api/admin/state", nil)
	recorder := httptest.NewRecorder()
	handlers.GetState(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	providers := payload["providers"].([]any)
	provider := providers[0].(map[string]any)
	if got := provider["api_key"]; got != "" && got != nil {
		t.Fatalf("expected provider api_key to be redacted, got %v", got)
	}
	if provider["api_key_preview"] == "" {
		t.Fatalf("expected provider api_key preview, got %+v", provider)
	}

	integrations := payload["integrations"].([]any)
	integrationPayload := integrations[0].(map[string]any)
	if got := integrationPayload["access_key"]; got != "" && got != nil {
		t.Fatalf("expected integration access_key to be redacted, got %v", got)
	}
	if got := integrationPayload["relay_api_key"]; got != "" && got != nil {
		t.Fatalf("expected integration relay_api_key to be redacted, got %v", got)
	}
	if integrationPayload["access_key_preview"] == "" || integrationPayload["relay_api_key_preview"] == "" {
		t.Fatalf("expected integration previews, got %+v", integrationPayload)
	}

	gatewayKeys := payload["gateway_keys"].([]any)
	key := gatewayKeys[0].(map[string]any)
	if _, ok := key["secret_hash"]; ok {
		t.Fatalf("did not expect secret_hash in admin response: %+v", key)
	}
	if _, ok := key["secret_lookup_hash"]; ok {
		t.Fatalf("did not expect secret_lookup_hash in admin response: %+v", key)
	}
}

func TestCreateGatewayKeyRejectsWeakCustomSecret(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.DefaultState())
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/gateway-keys", strings.NewReader(`{"name":"weak","secret":"short"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.CreateGatewayKey(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for weak secret, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminMiddlewareRejectsEmptyConfiguredToken(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.DefaultState())
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/meta", nil)
	recorder := httptest.NewRecorder()
	handlers.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected service unavailable for empty admin token, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCreateRouteRejectsDuplicateAliasCaseInsensitive(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		ModelRoutes: []model.ModelRoute{{
			Alias: "gpt-5.4",
			Targets: []model.RouteTarget{{
				ID:               "target-1",
				AccountID:        "provider-1",
				Weight:           1,
				Enabled:          true,
				MarkupMultiplier: 1,
			}},
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/routes", strings.NewReader(`{"alias":"GPT-5.4","targets":[]}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.CreateRoute(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for duplicate route alias, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDeleteProviderPrunesEmptyRoutesAndGatewayKeyReferences(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, time.April, 1, 8, 0, 0, 0, time.UTC)
	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Providers: []model.Provider{{
			ID:      "provider-1",
			Name:    "provider",
			Kind:    model.ProviderKindOpenAICompatible,
			BaseURL: "https://provider.example",
			APIKey:  "provider-key",
			Enabled: true,
		}},
		ModelRoutes: []model.ModelRoute{
			{
				Alias: "gpt-5.4",
				Targets: []model.RouteTarget{{
					ID:               "target-1",
					AccountID:        "provider-1",
					Weight:           1,
					Enabled:          true,
					MarkupMultiplier: 1,
				}},
			},
			{
				Alias: "claude-3.7",
				Targets: []model.RouteTarget{{
					ID:               "target-2",
					AccountID:        "provider-2",
					Weight:           1,
					Enabled:          true,
					MarkupMultiplier: 1,
				}},
			},
		},
		GatewayKeys: []model.GatewayKey{{
			ID:            "gk-1",
			Name:          "gateway",
			Enabled:       true,
			AllowedModels: []string{"gpt-5.4", "claude-3.7"},
			UpdatedAt:     updatedAt,
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodDelete, "/api/admin/providers/provider-1", nil)
	req.SetPathValue("id", "provider-1")
	recorder := httptest.NewRecorder()
	handlers.DeleteProvider(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected no content, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	if _, ok := saved.FindRoute("gpt-5.4"); ok {
		t.Fatalf("expected empty route to be pruned after provider deletion")
	}
	key, ok := saved.FindGatewayKey("gk-1")
	if !ok {
		t.Fatal("expected gateway key to remain")
	}
	if len(key.AllowedModels) != 1 || key.AllowedModels[0] != "claude-3.7" {
		t.Fatalf("expected deleted route alias to be removed from gateway key references, got %+v", key.AllowedModels)
	}
	if !key.UpdatedAt.After(updatedAt) {
		t.Fatalf("expected gateway key updated_at to advance after allowed model pruning, got %s", key.UpdatedAt)
	}
}

func TestUpdateGatewayKeyRotatesSecretAndEditableFields(t *testing.T) {
	t.Parallel()

	hash, err := model.HashGatewaySecret("original-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		GatewayKeys: []model.GatewayKey{{
			ID:            "gk-1",
			Name:          "original",
			SecretHash:    hash,
			SecretPreview: model.SecretPreview("original-secret-123"),
			Enabled:       true,
		}},
	})
	handlers := New(config.Config{GatewaySecretLookupPepper: "pepper"}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/gateway-keys/gk-1", strings.NewReader(`{
		"name":"rotated",
		"secret":"replacement-secret-456",
		"enabled":false,
		"max_concurrency":4,
		"rate_limit_rpm":18,
		"daily_budget_usd":2.5,
		"allowed_models":["gpt-5.4"],
		"allowed_protocols":["openai.chat"],
		"notes":"updated"
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "gk-1")
	recorder := httptest.NewRecorder()
	handlers.UpdateGatewayKey(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["secret"] != "replacement-secret-456" {
		t.Fatalf("expected rotated secret in response, got %+v", response)
	}

	saved := fileStore.Snapshot()
	key, ok := saved.FindGatewayKey("gk-1")
	if !ok {
		t.Fatal("expected updated gateway key")
	}
	if key.Name != "rotated" || key.Enabled || key.MaxConcurrency != 4 || key.RateLimitRPM != 18 {
		t.Fatalf("expected gateway key fields to be updated, got %+v", key)
	}
	if !model.VerifyGatewaySecret(key.SecretHash, "replacement-secret-456") {
		t.Fatalf("expected gateway secret hash to be rotated")
	}
	if key.SecretLookupHash == "" {
		t.Fatalf("expected lookup hash to be populated")
	}
}

func TestUpdateGatewayKeyClearsExpiresAtWhenNull(t *testing.T) {
	t.Parallel()

	hash, err := model.HashGatewaySecret("original-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	expiresAt := time.Date(2026, time.April, 10, 12, 0, 0, 0, time.UTC)
	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		GatewayKeys: []model.GatewayKey{{
			ID:         "gk-1",
			Name:       "expiring",
			SecretHash: hash,
			Enabled:    true,
			ExpiresAt:  &expiresAt,
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/gateway-keys/gk-1", strings.NewReader(`{"expires_at":null}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "gk-1")
	recorder := httptest.NewRecorder()
	handlers.UpdateGatewayKey(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	key, ok := saved.FindGatewayKey("gk-1")
	if !ok {
		t.Fatal("expected updated gateway key")
	}
	if key.ExpiresAt != nil {
		t.Fatalf("expected expires_at to be cleared, got %v", *key.ExpiresAt)
	}
}

func TestUpdateGatewayKeyClearsAllowedRestrictionsWhenNull(t *testing.T) {
	t.Parallel()

	hash, err := model.HashGatewaySecret("original-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		GatewayKeys: []model.GatewayKey{{
			ID:               "gk-1",
			Name:             "restricted",
			SecretHash:       hash,
			SecretLookupHash: "lookup",
			Enabled:          true,
			AllowedModels:    []string{"gpt-5.4"},
			AllowedProtocols: []model.Protocol{model.ProtocolOpenAIChat},
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/gateway-keys/gk-1", strings.NewReader(`{"allowed_models":null,"allowed_protocols":null}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "gk-1")
	recorder := httptest.NewRecorder()
	handlers.UpdateGatewayKey(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	key, ok := saved.FindGatewayKey("gk-1")
	if !ok {
		t.Fatal("expected updated gateway key")
	}
	if len(key.AllowedModels) != 0 {
		t.Fatalf("expected allowed_models to be cleared, got %+v", key.AllowedModels)
	}
	if len(key.AllowedProtocols) != 0 {
		t.Fatalf("expected allowed_protocols to be cleared, got %+v", key.AllowedProtocols)
	}
}

func TestSyncIntegrationFailureDoesNotPersistSnapshotError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"upstream failed"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Integrations: []model.Integration{{
			ID:        "integration-1",
			Name:      "NewAPI",
			Kind:      model.IntegrationKindNewAPI,
			BaseURL:   server.URL,
			UserID:    "132",
			AccessKey: "token",
			Enabled:   true,
			Snapshot: model.IntegrationSnapshot{
				LastError: "keep-me",
			},
		}},
	})

	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))
	req := httptest.NewRequest(http.MethodPost, "/api/admin/integrations/integration-1/sync", nil)
	req.SetPathValue("id", "integration-1")
	recorder := httptest.NewRecorder()
	handlers.SyncIntegration(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("expected bad gateway, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	if saved.Integrations[0].Snapshot.LastError != "keep-me" {
		t.Fatalf("expected sync failure not to overwrite persisted snapshot error, got %+v", saved.Integrations[0].Snapshot)
	}
}

func TestPutStatePreservesExistingRequestHistory(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		RequestHistory: []model.RequestRecord{{
			ID:         "req-original",
			RouteAlias: "gpt-5.4",
			StartedAt:  time.Date(2026, time.April, 2, 0, 0, 0, 0, time.UTC),
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/state", strings.NewReader(`{
		"version":"2026-04-01",
		"providers":[],
		"model_routes":[],
		"pricing_profiles":[],
		"integrations":[],
		"gateway_keys":[],
		"request_history":[{"id":"req-overwrite","route_alias":"malicious"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.PutState(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	if len(saved.RequestHistory) != 1 {
		t.Fatalf("expected request history to stay unchanged, got %+v", saved.RequestHistory)
	}
	if saved.RequestHistory[0].ID != "req-original" {
		t.Fatalf("expected existing request history to be preserved, got %+v", saved.RequestHistory)
	}
}

func TestPutStatePreservesGatewayKeySecrets(t *testing.T) {
	t.Parallel()

	hash, err := model.HashGatewaySecret("original-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		GatewayKeys: []model.GatewayKey{{
			ID:               "gk-1",
			Name:             "gateway",
			SecretHash:       hash,
			SecretLookupHash: "lookup-hash",
			SecretPreview:    model.SecretPreview("original-secret-123"),
			Enabled:          true,
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/state", strings.NewReader(`{
		"version":"2026-04-01",
		"providers":[],
		"model_routes":[],
		"pricing_profiles":[],
		"integrations":[],
		"gateway_keys":[{
			"id":"gk-1",
			"name":"gateway",
			"secret_hash":"forged-hash",
			"secret_lookup_hash":"forged-lookup",
			"secret_preview":"forged-preview",
			"enabled":true
		}],
		"request_history":[]
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.PutState(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	key, ok := saved.FindGatewayKey("gk-1")
	if !ok {
		t.Fatal("expected gateway key to remain")
	}
	if key.SecretHash == "" || key.SecretLookupHash == "" {
		t.Fatalf("expected gateway key secrets to be preserved, got %+v", key)
	}
	if key.SecretHash == "forged-hash" || key.SecretLookupHash == "forged-lookup" || key.SecretPreview == "forged-preview" {
		t.Fatalf("expected compatibility state update to ignore incoming secret-derived fields, got %+v", key)
	}
	if !model.VerifyGatewaySecret(key.SecretHash, "original-secret-123") {
		t.Fatalf("expected preserved secret hash to remain valid")
	}
}

func TestPutStateDoesNotBorrowGatewayKeySecretsAcrossDifferentIDs(t *testing.T) {
	t.Parallel()

	hash, err := model.HashGatewaySecret("original-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		GatewayKeys: []model.GatewayKey{{
			ID:               "gk-1",
			Name:             "gateway",
			SecretHash:       hash,
			SecretLookupHash: "lookup-hash",
			SecretPreview:    model.SecretPreview("original-secret-123"),
			Enabled:          true,
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/state", strings.NewReader(`{
		"version":"2026-04-01",
		"providers":[],
		"model_routes":[],
		"pricing_profiles":[],
		"integrations":[],
		"gateway_keys":[{"id":"gk-2","name":"new-key","enabled":true}],
		"request_history":[]
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.PutState(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	key, ok := saved.FindGatewayKey("gk-2")
	if !ok {
		t.Fatal("expected replacement gateway key to be present")
	}
	if key.SecretHash != "" || key.SecretLookupHash != "" || key.SecretPreview != "" {
		t.Fatalf("expected unmatched gateway key IDs not to inherit existing secrets, got %+v", key)
	}
}

func TestPutStateDoesNotBorrowCredentialSecretsAcrossDifferentIDs(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Providers: []model.Provider{{
			ID:      "provider-1",
			Name:    "provider",
			Kind:    model.ProviderKindOpenAICompatible,
			BaseURL: "https://provider.example",
			APIKey:  "provider-key",
			Enabled: true,
			Credentials: []model.ProviderCredential{{
				ID:      "cred-1",
				Label:   "primary",
				APIKey:  "credential-secret",
				Enabled: true,
			}},
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/state", strings.NewReader(`{
		"version":"2026-04-01",
		"providers":[{
			"id":"provider-1",
			"name":"provider",
			"kind":"openai-compatible",
			"base_url":"https://provider.example",
			"enabled":true,
			"credentials":[{"id":"cred-2","label":"replacement","enabled":true}]
		}],
		"model_routes":[],
		"pricing_profiles":[],
		"integrations":[],
		"gateway_keys":[],
		"request_history":[]
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.PutState(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}

	saved := fileStore.Snapshot()
	provider, ok := saved.FindProvider("provider-1")
	if !ok {
		t.Fatal("expected provider to remain")
	}
	if len(provider.Credentials) != 1 {
		t.Fatalf("expected one credential, got %+v", provider.Credentials)
	}
	if provider.Credentials[0].ID != "cred-2" {
		t.Fatalf("expected replacement credential to be kept, got %+v", provider.Credentials[0])
	}
	if provider.Credentials[0].APIKey != "" {
		t.Fatalf("expected unmatched credential IDs not to inherit API keys, got %+v", provider.Credentials[0])
	}
}

func TestSyncAllIntegrationsAppliesSuccessfulResultsEvenWhenSomeFail(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/api/user/self":
			switch token {
			case "ok-token":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data": map[string]any{
						"quota":      64,
						"used_quota": 8,
					},
				})
			case "fail-token":
				http.Error(w, `{"message":"upstream failed"}`, http.StatusBadGateway)
			default:
				t.Fatalf("unexpected token for self endpoint: %s", token)
			}
		case "/api/user/models":
			switch token {
			case "ok-token":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data":    []map[string]any{{"id": "gpt-5.4"}},
				})
			default:
				http.Error(w, `{"message":"upstream failed"}`, http.StatusBadGateway)
			}
		case "/api/pricing":
			if token != "" {
				t.Fatalf("expected pricing endpoint to be unauthenticated, got token %q", token)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    []map[string]any{{"model_name": "gpt-5.4", "input_per_million": 2.5, "output_per_million": 10}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Integrations: []model.Integration{
			{
				ID:               "integration-ok",
				Name:             "OK",
				Kind:             model.IntegrationKindNewAPI,
				BaseURL:          server.URL,
				AccessKey:        "ok-token",
				Enabled:          true,
				AutoCreateRoutes: true,
			},
			{
				ID:        "integration-fail",
				Name:      "FAIL",
				Kind:      model.IntegrationKindNewAPI,
				BaseURL:   server.URL,
				AccessKey: "fail-token",
				Enabled:   true,
				Snapshot: model.IntegrationSnapshot{
					LastError: "keep-me",
				},
			},
		},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/integrations/sync", nil)
	recorder := httptest.NewRecorder()
	handlers.SyncAllIntegrations(recorder, req)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("expected multistatus for partial sync, got %d body=%s", recorder.Code, recorder.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	failures, ok := response["failures"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("expected one failure entry, got %+v", response)
	}

	saved := fileStore.Snapshot()
	okIntegration, found := saved.FindIntegration("integration-ok")
	if !found || len(okIntegration.Snapshot.ModelNames) != 1 || okIntegration.Snapshot.ModelNames[0] != "gpt-5.4" {
		t.Fatalf("expected successful integration sync to be applied, got %+v", okIntegration.Snapshot)
	}
	failIntegration, found := saved.FindIntegration("integration-fail")
	if !found {
		t.Fatal("expected failing integration to remain")
	}
	if failIntegration.Snapshot.LastError != "keep-me" {
		t.Fatalf("expected failed integration snapshot to stay untouched, got %+v", failIntegration.Snapshot)
	}
	if _, ok := saved.FindRoute("gpt-5.4"); !ok {
		t.Fatalf("expected successful sync to create route")
	}
}

func TestRunCheckinsPersistsCheckinErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/checkin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"stats": map[string]any{
						"is_checked_in": false,
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/user/checkin":
			http.Error(w, `{"message":"upstream failed"}`, http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Integrations: []model.Integration{
			{
				ID:        "integration-1",
				Name:      "NewAPI",
				Kind:      model.IntegrationKindNewAPI,
				BaseURL:   server.URL,
				UserID:    "132",
				AccessKey: "token",
				Enabled:   true,
				Snapshot: model.IntegrationSnapshot{
					SupportsCheckin: true,
				},
			},
		},
	})

	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))
	handlers.RunCheckins(context.Background())

	saved := fileStore.Snapshot()
	snapshot := saved.Integrations[0].Snapshot
	if snapshot.LastError == "" {
		t.Fatalf("expected checkin error to be persisted")
	}
	if snapshot.LastCheckinAt != nil {
		t.Fatalf("expected failed checkin not to set last_checkin_at")
	}
	if snapshot.LastCheckinResult != "checkin failed" {
		t.Fatalf("expected failure message to be recorded, got %q", snapshot.LastCheckinResult)
	}
}

func TestRunCheckinsDoesNotBlockSnapshotsWhileWaitingOnUpstream(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/user/checkin":
			once.Do(func() { close(started) })
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"stats": map[string]any{
						"is_checked_in": false,
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/user/checkin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_awarded": 8,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Integrations: []model.Integration{
			{
				ID:        "integration-1",
				Name:      "NewAPI",
				Kind:      model.IntegrationKindNewAPI,
				BaseURL:   server.URL,
				UserID:    "132",
				AccessKey: "token",
				Enabled:   true,
				Snapshot: model.IntegrationSnapshot{
					SupportsCheckin: true,
				},
			},
		},
	})

	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		handlers.RunCheckins(context.Background())
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected upstream checkin probe to start")
	}

	snapshotDone := make(chan model.State, 1)
	go func() {
		snapshotDone <- fileStore.Snapshot()
	}()

	select {
	case snapshot := <-snapshotDone:
		if len(snapshot.Integrations) != 1 {
			t.Fatalf("unexpected snapshot content: %+v", snapshot.Integrations)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("snapshot blocked while upstream checkin was in flight")
	}

	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run checkins did not complete after upstream release")
	}
}

func TestBuildOpenAPISpecCoversAdminRoutes(t *testing.T) {
	t.Parallel()

	paths := buildOpenAPISpec()["paths"].(map[string]any)
	required := []string{
		"/api/admin/state",
		"/api/admin/meta",
		"/api/admin/integrations/sync",
		"/api/admin/providers",
		"/api/admin/providers/{id}",
		"/api/admin/routes",
		"/api/admin/routes/{alias}",
		"/api/admin/pricing-profiles",
		"/api/admin/pricing-profiles/{id}",
		"/api/admin/integrations",
		"/api/admin/integrations/{id}",
		"/api/admin/integrations/{id}/sync",
		"/api/admin/integrations/{id}/checkin",
		"/api/admin/gateway-keys",
		"/api/admin/gateway-keys/{id}",
		"/api/admin/request-history",
		"/api/admin/request-history/{id}/attempts",
		"/api/admin/openapi.json",
		"/api/admin/maintenance/probes",
	}
	for _, path := range required {
		if _, ok := paths[path]; !ok {
			t.Fatalf("expected openapi spec to contain %s", path)
		}
	}
}

func openTestStore(t *testing.T, state model.State) *store.FileStore {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.json")
	fileStore, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace store state: %v", err)
	}
	return fileStore
}
