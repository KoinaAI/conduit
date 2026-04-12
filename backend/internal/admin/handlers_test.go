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
			Enabled: true,
			Endpoints: []model.ProviderEndpoint{{
				ID:      "endpoint-1",
				BaseURL: "https://provider.example",
				Enabled: true,
				Weight:  1,
				Headers: map[string]string{},
			}},
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
	if _, ok := provider["api_key"]; ok {
		t.Fatalf("did not expect legacy provider api_key field, got %+v", provider)
	}
	credentials := provider["credentials"].([]any)
	credential := credentials[0].(map[string]any)
	if got := credential["api_key"]; got != "" && got != nil {
		t.Fatalf("expected provider credential api_key to be redacted, got %v", got)
	}
	if credential["api_key_preview"] == "" {
		t.Fatalf("expected provider credential api_key preview, got %+v", credential)
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

func TestCreateProviderRejectsReadOnlyFields(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.DefaultState())
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers", strings.NewReader(`{
		"name":"Provider",
		"kind":"openai-compatible",
		"enabled":true,
		"weight":1,
		"timeout_seconds":180,
		"default_markup_multiplier":1,
		"capabilities":["openai.chat","openai.responses"],
		"endpoints":[{"id":"endpoint-1","base_url":"https://provider.example/v1","enabled":true,"weight":1,"priority":0,"headers":{}}],
		"credentials":[{"id":"credential-1","api_key":"provider-secret-key","enabled":true,"weight":1,"headers":{}}],
		"created_at":"2026-01-01T00:00:00Z"
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.CreateProvider(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for read-only provider fields, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "created_at") {
		t.Fatalf("expected unknown-field error to mention created_at, got %s", recorder.Body.String())
	}
}

func TestUpdateIntegrationRejectsReadOnlyFields(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.State{
		Version: "2026-04-01",
		Integrations: []model.Integration{{
			ID:          "integration-1",
			Name:        "NewAPI",
			Kind:        model.IntegrationKindNewAPI,
			BaseURL:     "https://relay.example",
			AccessKey:   "access-secret-key",
			RelayAPIKey: "relay-secret-key",
			Enabled:     true,
			Snapshot: model.IntegrationSnapshot{
				Balance: 12,
			},
		}},
	})
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/integrations/integration-1", strings.NewReader(`{
		"name":"NewAPI",
		"kind":"newapi",
		"base_url":"https://relay.example",
		"access_key":"access-secret-key",
		"enabled":true,
		"auto_create_routes":true,
		"default_protocols":["openai.chat"],
		"default_markup_multiplier":1,
		"model_markup_overrides":{},
		"snapshot":{"balance":999}
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	req.SetPathValue("id", "integration-1")
	handlers.UpdateIntegration(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for read-only integration fields, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "snapshot") {
		t.Fatalf("expected unknown-field error to mention snapshot, got %s", recorder.Body.String())
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
		"integrations":[]
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

func TestPutStateRejectsReadOnlyFields(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.DefaultState())
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	req := httptest.NewRequest(http.MethodPut, "/api/admin/state", strings.NewReader(`{
		"version":"2026-04-01",
		"providers":[{"id":"provider-1","name":"Provider","kind":"openai-compatible","enabled":true,"weight":1,"timeout_seconds":180,"default_markup_multiplier":1,"capabilities":["openai.chat"],"headers":{},"endpoints":[{"id":"endpoint-1","base_url":"https://provider.example/v1","enabled":true,"weight":1,"priority":0,"headers":{}}],"credentials":[{"id":"credential-1","api_key":"provider-secret-key","enabled":true,"weight":1,"headers":{}}],"created_at":"2026-01-01T00:00:00Z"}],
		"model_routes":[],
		"pricing_profiles":[],
		"integrations":[],
		"request_history":[{"id":"req-overwrite"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handlers.PutState(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for read-only state fields, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "created_at") && !strings.Contains(body, "request_history") {
		t.Fatalf("expected unknown-field error to mention a read-only field, got %s", body)
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
