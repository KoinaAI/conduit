package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestResolveStatsWindowSupportsNamedAndDays(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.April, 4, 12, 0, 0, 0, time.UTC)

	today, err := ResolveStatsWindow("today", "", now)
	if err != nil {
		t.Fatalf("resolve today window: %v", err)
	}
	if today.Label != "today" || !today.StartAt.Equal(time.Date(2026, time.April, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected today window: %+v", today)
	}

	sevenDays, err := ResolveStatsWindow("", "7", now)
	if err != nil {
		t.Fatalf("resolve days window: %v", err)
	}
	if sevenDays.Label != "7d" || sevenDays.Days != 7 {
		t.Fatalf("unexpected 7-day window: %+v", sevenDays)
	}

	if _, err := ResolveStatsWindow("30d", "7", now); err == nil {
		t.Fatal("expected combining window and days to fail")
	}
	if _, err := ResolveStatsWindow("quarter", "", now); err == nil {
		t.Fatal("expected invalid named window to fail")
	}
	if _, err := ResolveStatsWindow("", "0", now); err == nil {
		t.Fatal("expected non-positive days to fail")
	}
}

func TestStatsReportsAggregateByWindowAndDimension(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	fileStore, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	state := model.DefaultState()
	state.GatewayKeys = []model.GatewayKey{
		{ID: "gk-1", Name: "Primary", Enabled: true},
		{ID: "gk-2", Name: "Fallback", Enabled: true},
	}
	if _, err := fileStore.Replace(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}

	now := time.Date(2026, time.April, 4, 12, 0, 0, 0, time.UTC)
	records := []model.RequestRecord{
		makeStatsRecord("req-1", now.Add(-48*time.Hour), "gk-1", "provider-1", "OpenAI", "gpt-5.4", 200, true, 100, 50, 150, 1.1),
		makeStatsRecord("req-2", now.Add(-24*time.Hour), "gk-1", "provider-1", "OpenAI", "gpt-5.4", 500, false, 20, 0, 20, 0.2),
		makeStatsRecord("req-3", now.Add(-3*time.Hour), "gk-2", "provider-2", "Anthropic", "claude-3.7", 200, false, 10, 40, 50, 0.6),
		makeStatsRecord("req-4", now.Add(-20*24*time.Hour), "gk-2", "provider-2", "Anthropic", "claude-3.5", 200, true, 30, 70, 100, 0.9),
		makeStatsRecord("req-5", now.Add(-40*24*time.Hour), "gk-1", "provider-1", "OpenAI", "gpt-4.1", 200, false, 1, 1, 2, 0.01),
	}
	for _, record := range records {
		if err := fileStore.AppendRequestRecord(record, nil, 100); err != nil {
			t.Fatalf("append request record %s: %v", record.ID, err)
		}
	}

	window7d, err := ResolveStatsWindow("7d", "", now)
	if err != nil {
		t.Fatalf("resolve 7d window: %v", err)
	}
	summary7d, err := fileStore.StatsSummary(window7d)
	if err != nil {
		t.Fatalf("summary 7d: %v", err)
	}
	if summary7d.Summary.RequestCount != 3 || summary7d.Summary.SuccessCount != 2 || summary7d.Summary.ErrorCount != 1 {
		t.Fatalf("unexpected 7d summary counts: %+v", summary7d.Summary)
	}
	if summary7d.Summary.TotalTokens != 220 || summary7d.Summary.FinalCost != 1.9 {
		t.Fatalf("unexpected 7d summary totals: %+v", summary7d.Summary)
	}

	window30d, err := ResolveStatsWindow("30d", "", now)
	if err != nil {
		t.Fatalf("resolve 30d window: %v", err)
	}
	summary30d, err := fileStore.StatsSummary(window30d)
	if err != nil {
		t.Fatalf("summary 30d: %v", err)
	}
	if summary30d.Summary.RequestCount != 4 || summary30d.Summary.TotalTokens != 320 || summary30d.Summary.FinalCost != 2.8 {
		t.Fatalf("unexpected 30d summary totals: %+v", summary30d.Summary)
	}

	byKey, err := fileStore.StatsByGatewayKey(window7d)
	if err != nil {
		t.Fatalf("by key: %v", err)
	}
	if len(byKey.Items) != 2 {
		t.Fatalf("expected 2 key rows, got %+v", byKey.Items)
	}
	if byKey.Items[0].ID != "gk-1" || byKey.Items[0].Name != "Primary" || byKey.Items[0].RequestCount != 2 {
		t.Fatalf("unexpected primary key stats: %+v", byKey.Items[0])
	}
	if byKey.Items[1].ID != "gk-2" || byKey.Items[1].FinalCost != 0.6 {
		t.Fatalf("unexpected fallback key stats: %+v", byKey.Items[1])
	}

	byProvider, err := fileStore.StatsByProvider(window7d)
	if err != nil {
		t.Fatalf("by provider: %v", err)
	}
	if len(byProvider.Items) != 2 {
		t.Fatalf("expected 2 provider rows, got %+v", byProvider.Items)
	}
	if byProvider.Items[0].ID != "provider-1" || byProvider.Items[0].Name != "OpenAI" || byProvider.Items[0].RequestCount != 2 {
		t.Fatalf("unexpected provider-1 stats: %+v", byProvider.Items[0])
	}

	byModel, err := fileStore.StatsByModel(window7d)
	if err != nil {
		t.Fatalf("by model: %v", err)
	}
	if len(byModel.Items) != 2 {
		t.Fatalf("expected 2 model rows, got %+v", byModel.Items)
	}
	if byModel.Items[0].ID != "gpt-5.4" || byModel.Items[0].RequestCount != 2 || byModel.Items[0].ErrorCount != 1 {
		t.Fatalf("unexpected gpt-5.4 stats: %+v", byModel.Items[0])
	}
	if byModel.Items[1].ID != "claude-3.7" || byModel.Items[1].SuccessCount != 1 {
		t.Fatalf("unexpected claude-3.7 stats: %+v", byModel.Items[1])
	}
}

func makeStatsRecord(
	id string,
	startedAt time.Time,
	gatewayKeyID, accountID, providerName, routeAlias string,
	statusCode int,
	stream bool,
	inputTokens, outputTokens, totalTokens int64,
	finalCost float64,
) model.RequestRecord {
	return model.RequestRecord{
		ID:           id,
		Protocol:     model.ProtocolOpenAIResponses,
		RouteAlias:   routeAlias,
		AccountID:    accountID,
		ProviderName: providerName,
		GatewayKeyID: gatewayKeyID,
		StatusCode:   statusCode,
		Stream:       stream,
		StartedAt:    startedAt,
		Usage: model.UsageSummary{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  totalTokens,
		},
		Billing: model.BillingSummary{
			Currency:  "USD",
			FinalCost: finalCost,
		},
	}
}
