package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

// requirePostgresLocator returns a Postgres connection URL pulled from the
// CONDUIT_TEST_POSTGRES_URL environment variable, skipping the calling test
// when it is unset. CI provides this for the Postgres matrix job; local
// developers can opt in by exporting it manually.
func requirePostgresLocator(t *testing.T) string {
	t.Helper()
	locator := strings.TrimSpace(os.Getenv("CONDUIT_TEST_POSTGRES_URL"))
	if locator == "" {
		t.Skip("CONDUIT_TEST_POSTGRES_URL not set; skipping Postgres-backed test")
	}
	return locator
}

func openPostgresStore(t *testing.T) *FileStore {
	t.Helper()
	locator := requirePostgresLocator(t)
	store, err := Open(locator, WithRequestHistoryLimit(50))
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort schema teardown so successive runs see a clean state.
		// We rely on Close() to drop the connection pool; the actual data is
		// purged via the freshening below before the next test inserts.
		_ = store.Close()
	})

	// Reset persisted state between runs so tests are deterministic. We can't
	// drop the DB itself (we don't own it), so we replace state with the
	// default and clear request history.
	if _, err := store.Replace(model.DefaultState()); err != nil {
		t.Fatalf("reset postgres state: %v", err)
	}
	return store
}

func TestPostgresBackendRoundTripsState(t *testing.T) {
	store := openPostgresStore(t)

	hash, err := model.HashGatewaySecret("postgres-secret-789")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	state := model.DefaultState()
	state.Providers = []model.Provider{{
		ID:           "provider-pg",
		Name:         "Postgres Provider",
		Capabilities: []model.Protocol{model.ProtocolOpenAIChat},
	}}
	state.GatewayKeys = []model.GatewayKey{{
		ID:               "gk-pg",
		Name:             "pg-key",
		SecretHash:       hash,
		SecretLookupHash: "lookup-pg",
		SecretPreview:    model.SecretPreview("postgres-secret-789"),
		Enabled:          true,
	}}
	if _, err := store.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	if err := store.AppendRequestRecord(model.RequestRecord{ID: "req-pg-1"}, nil, 5); err != nil {
		t.Fatalf("append record: %v", err)
	}

	snap := store.Snapshot()
	if len(snap.Providers) != 1 || snap.Providers[0].ID != "provider-pg" {
		t.Fatalf("expected provider to persist, got %+v", snap.Providers)
	}
	key, ok := snap.FindGatewayKey("gk-pg")
	if !ok || !model.VerifyGatewaySecret(key.SecretHash, "postgres-secret-789") {
		t.Fatalf("expected gateway key to persist with valid secret hash, got %+v", key)
	}
	if len(snap.RequestHistory) != 1 || snap.RequestHistory[0].ID != "req-pg-1" {
		t.Fatalf("expected request history to persist, got %+v", snap.RequestHistory)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("postgres ping failed: %v", err)
	}

	counts := store.HealthCounts(time.Now().UTC())
	if counts.Providers != 1 || counts.GatewayKeysTotal != 1 || counts.RequestHistoryItems != 1 {
		t.Fatalf("unexpected health counts: %+v", counts)
	}
}

func TestPostgresBackendBackupRejected(t *testing.T) {
	store := openPostgresStore(t)
	if _, err := store.Backup(t.TempDir(), 1); err == nil {
		t.Fatal("expected backup against postgres to be rejected")
	}
}
