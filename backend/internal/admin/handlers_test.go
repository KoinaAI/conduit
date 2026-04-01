package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

	handlers := New(config.Config{}, fileStore, integration.NewService())
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

	handlers := New(config.Config{}, fileStore, integration.NewService())
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

	handlers := New(config.Config{}, fileStore, integration.NewService())
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
