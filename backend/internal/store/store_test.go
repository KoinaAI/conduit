package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestAppendRequestRecordTrimsHistoryAndPreservesRoutingState(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	fileStore, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	state := model.DefaultState()
	state.Providers = []model.Provider{
		{
			ID:           "provider-1",
			Name:         "Provider 1",
			Capabilities: []model.Protocol{model.ProtocolOpenAIChat},
			Headers:      map[string]string{"x-test": "a"},
		},
	}
	state.ModelRoutes = []model.ModelRoute{
		{
			Alias: "gpt-5",
			Targets: []model.RouteTarget{
				{
					ID:        "target-1",
					AccountID: "provider-1",
					Protocols: []model.Protocol{model.ProtocolOpenAIResponses},
				},
			},
		},
	}
	state.PricingProfiles = []model.PricingProfile{{ID: "pricing-1", Name: "Pricing 1"}}

	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	if err := fileStore.AppendRequestRecord(model.RequestRecord{ID: "req-1"}, nil, 1); err != nil {
		t.Fatalf("append first record: %v", err)
	}
	if err := fileStore.AppendRequestRecord(model.RequestRecord{ID: "req-2"}, nil, 1); err != nil {
		t.Fatalf("append second record: %v", err)
	}

	saved := fileStore.Snapshot()
	if len(saved.RequestHistory) != 1 {
		t.Fatalf("expected trimmed history length 1, got %d", len(saved.RequestHistory))
	}
	if saved.RequestHistory[0].ID != "req-2" {
		t.Fatalf("expected most recent record to be retained, got %q", saved.RequestHistory[0].ID)
	}
	if saved.Providers[0].Headers["x-test"] != "a" {
		t.Fatalf("provider headers changed unexpectedly")
	}
	if saved.ModelRoutes[0].Targets[0].Protocols[0] != model.ProtocolOpenAIResponses {
		t.Fatalf("route target protocols changed unexpectedly")
	}
	if saved.PricingProfiles[0].Name != "Pricing 1" {
		t.Fatalf("pricing profile changed unexpectedly")
	}
}

func TestOpenRoundTripPreservesGatewayKeySecretHash(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	fileStore, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	hash, err := model.HashGatewaySecret("gateway-secret-123")
	if err != nil {
		t.Fatalf("hash secret: %v", err)
	}
	state := model.DefaultState()
	state.GatewayKeys = []model.GatewayKey{{
		ID:               "gk-1",
		Name:             "gateway",
		SecretHash:       hash,
		SecretLookupHash: "lookup",
		SecretPreview:    model.SecretPreview("gateway-secret-123"),
		Enabled:          true,
	}}
	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}
	if err := fileStore.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	saved := reopened.Snapshot()
	key, ok := saved.FindGatewayKey("gk-1")
	if !ok {
		t.Fatal("expected gateway key after reopen")
	}
	if key.SecretHash == "" {
		t.Fatalf("expected secret hash to persist across reopen, got %+v", key)
	}
	if !model.VerifyGatewaySecret(key.SecretHash, "gateway-secret-123") {
		t.Fatalf("expected persisted secret hash to stay valid")
	}
}

func TestOpenHonorsConfiguredRequestHistoryLimitOnReload(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	fileStore, err := Open(path, WithRequestHistoryLimit(5))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	for index := 0; index < 8; index++ {
		recordID := "req-" + strconv.Itoa(index+1)
		if err := fileStore.AppendRequestRecord(model.RequestRecord{ID: recordID, StartedAt: time.Now().UTC().Add(time.Duration(index) * time.Millisecond)}, nil, 5); err != nil {
			t.Fatalf("append record %s: %v", recordID, err)
		}
	}
	if err := fileStore.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := Open(path, WithRequestHistoryLimit(5))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	saved := reopened.Snapshot()
	if len(saved.RequestHistory) != 5 {
		t.Fatalf("expected 5 records after reopen, got %d", len(saved.RequestHistory))
	}
	if saved.RequestHistory[0].ID != "req-4" || saved.RequestHistory[4].ID != "req-8" {
		t.Fatalf("expected most recent 5 records after reopen, got %+v", saved.RequestHistory)
	}
}

func TestBackupCreatesRotatingSnapshots(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "gateway.db")
	fileStore, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	if _, err := fileStore.Replace(model.DefaultState()); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	backupDir := filepath.Join(tempDir, "backups")
	for index := 0; index < 3; index++ {
		backupPath, err := fileStore.Backup(backupDir, 2)
		if err != nil {
			t.Fatalf("backup %d: %v", index, err)
		}
		if _, err := os.Stat(backupPath); err != nil {
			t.Fatalf("expected backup file %q to exist: %v", backupPath, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected backup retention to keep 2 snapshots, got %d", len(entries))
	}
}

func TestDetectBackendRecognizesPostgresLocator(t *testing.T) {
	t.Parallel()

	backend, err := detectBackend("postgres://conduit:secret@db.example/conduit?sslmode=disable")
	if err != nil {
		t.Fatalf("detect backend: %v", err)
	}
	if backend != backendPostgres {
		t.Fatalf("expected postgres backend, got %q", backend)
	}
}

func TestRebindQueryConvertsQuestionPlaceholdersToDollarSyntax(t *testing.T) {
	t.Parallel()

	query := `INSERT INTO request_records (id, payload, started_at) VALUES (?, ?, ?) ON CONFLICT(id) DO UPDATE SET payload = excluded.payload`
	rewritten := rebindQuery(query)
	if strings.Contains(rewritten, "?") {
		t.Fatalf("expected rebinding to remove question placeholders, got %q", rewritten)
	}
	if !strings.Contains(rewritten, "$1") || !strings.Contains(rewritten, "$2") || !strings.Contains(rewritten, "$3") {
		t.Fatalf("expected rebinding to introduce dollar placeholders, got %q", rewritten)
	}
}

func TestBackupRejectsPostgresBackends(t *testing.T) {
	t.Parallel()

	fileStore := &FileStore{backend: backendPostgres}
	if _, err := fileStore.Backup(t.TempDir(), 1); err == nil {
		t.Fatal("expected postgres backup to be rejected")
	}
}
