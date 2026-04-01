package store

import (
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
