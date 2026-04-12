package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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

func TestOpenRejectsLegacyJSONStateFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":"2026-04-01"}`), 0o600); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("expected legacy JSON state files to be rejected")
	}
	if _, err := os.Stat(path + ".legacy.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("did not expect legacy backup file to be created, stat err=%v", err)
	}
}

func TestStorePersistsGatewayKeyHashesAcrossReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gateway.db")
	firstStore, err := Open(path)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}

	state := model.DefaultState()
	state.GatewayKeys = []model.GatewayKey{{
		ID:               "gk-1",
		Name:             "gateway",
		SecretHash:       "bcrypt-hash",
		SecretLookupHash: "lookup-hash",
		SecretPreview:    "uag-12...abcd",
		Enabled:          true,
	}}
	if _, err := firstStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}

	snapshot := reopened.Snapshot()
	if len(snapshot.GatewayKeys) != 1 {
		t.Fatalf("expected one gateway key after reopen, got %+v", snapshot.GatewayKeys)
	}
	if snapshot.GatewayKeys[0].SecretHash != "bcrypt-hash" {
		t.Fatalf("expected secret hash to survive reopen, got %+v", snapshot.GatewayKeys[0])
	}
}
