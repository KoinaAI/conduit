package store

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestQueryRequestHistorySupportsFiltersAndCursor(t *testing.T) {
	t.Parallel()

	fileStore, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	now := time.Now().UTC()
	records := []model.RequestRecord{
		{
			ID:              "req-1",
			Protocol:        model.ProtocolOpenAIChat,
			RouteAlias:      "gpt-5.4",
			AccountID:       "provider-1",
			ProviderName:    "OpenAI",
			GatewayKeyID:    "gk-1",
			ClientSessionID: "sess-a",
			StatusCode:      200,
			StartedAt:       now.Add(-3 * time.Minute),
		},
		{
			ID:              "req-2",
			Protocol:        model.ProtocolOpenAIResponses,
			RouteAlias:      "gpt-5.4",
			AccountID:       "provider-1",
			ProviderName:    "OpenAI",
			GatewayKeyID:    "gk-1",
			ClientSessionID: "sess-b",
			StatusCode:      502,
			Error:           "upstream timeout",
			StartedAt:       now.Add(-2 * time.Minute),
		},
		{
			ID:              "req-3",
			Protocol:        model.ProtocolAnthropic,
			RouteAlias:      "claude-3.7",
			AccountID:       "provider-2",
			ProviderName:    "Anthropic",
			GatewayKeyID:    "gk-2",
			ClientSessionID: "sess-c",
			StatusCode:      200,
			StartedAt:       now.Add(-1 * time.Minute),
		},
	}
	for _, record := range records {
		if err := fileStore.AppendRequestRecord(record, nil, 10); err != nil {
			t.Fatalf("append %s: %v", record.ID, err)
		}
	}

	firstPage, err := fileStore.QueryRequestHistory(RequestHistoryQuery{
		Limit:      1,
		RouteAlias: "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("query first page: %v", err)
	}
	if len(firstPage.Items) != 1 || firstPage.Items[0].ID != "req-2" {
		t.Fatalf("unexpected first page: %+v", firstPage)
	}
	if !firstPage.HasMore || firstPage.NextCursor == "" {
		t.Fatalf("expected next cursor on first page, got %+v", firstPage)
	}

	secondPage, err := fileStore.QueryRequestHistory(RequestHistoryQuery{
		Limit:      1,
		RouteAlias: "gpt-5.4",
		Cursor:     firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("query second page: %v", err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID != "req-1" {
		t.Fatalf("unexpected second page: %+v", secondPage)
	}
	if secondPage.HasMore {
		t.Fatalf("did not expect more items on second page: %+v", secondPage)
	}

	filtered, err := fileStore.QueryRequestHistory(RequestHistoryQuery{
		Limit:     10,
		SessionID: "sess-b",
		HasError:  boolPointer(true),
	})
	if err != nil {
		t.Fatalf("query filtered history: %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].ID != "req-2" {
		t.Fatalf("unexpected filtered page: %+v", filtered)
	}
}

func TestActiveSessionsAggregatesRecentRequests(t *testing.T) {
	t.Parallel()

	fileStore, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	now := time.Now().UTC()
	for _, record := range []model.RequestRecord{
		{
			ID:              "req-1",
			RouteAlias:      "gpt-5.4",
			AccountID:       "provider-1",
			ProviderName:    "OpenAI",
			GatewayKeyID:    "gk-1",
			ClientSessionID: "sess-a",
			StatusCode:      200,
			StartedAt:       now.Add(-4 * time.Minute),
			Usage:           model.UsageSummary{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			Billing:         model.BillingSummary{FinalCost: 0.12},
			DurationMS:      800,
		},
		{
			ID:              "req-2",
			RouteAlias:      "gpt-5.4",
			AccountID:       "provider-1",
			ProviderName:    "OpenAI",
			GatewayKeyID:    "gk-1",
			ClientSessionID: "sess-a",
			StatusCode:      200,
			StartedAt:       now.Add(-2 * time.Minute),
			Usage:           model.UsageSummary{InputTokens: 8, OutputTokens: 7, TotalTokens: 15},
			Billing:         model.BillingSummary{FinalCost: 0.11},
			DurationMS:      900,
		},
		{
			ID:              "req-3",
			RouteAlias:      "claude-3.7",
			AccountID:       "provider-2",
			ProviderName:    "Anthropic",
			GatewayKeyID:    "gk-2",
			ClientSessionID: "sess-b",
			StatusCode:      502,
			StartedAt:       now.Add(-20 * time.Minute),
		},
	} {
		if err := fileStore.AppendRequestRecord(record, nil, 10); err != nil {
			t.Fatalf("append %s: %v", record.ID, err)
		}
	}

	items, err := fileStore.ActiveSessions(now, 15*time.Minute, 10)
	if err != nil {
		t.Fatalf("active sessions: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected only one active session, got %+v", items)
	}
	if items[0].SessionID != "sess-a" || items[0].RequestCount != 2 {
		t.Fatalf("unexpected session summary: %+v", items[0])
	}
	if items[0].TotalTokens != 30 || items[0].FinalCostUSD < 0.229 || items[0].FinalCostUSD > 0.231 {
		t.Fatalf("unexpected aggregated usage: %+v", items[0])
	}
}

func TestActiveSessionsScansFullWindowInsteadOfSamplingRecentRows(t *testing.T) {
	t.Parallel()

	fileStore, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	now := time.Now().UTC()
	for index := 0; index < 24; index++ {
		record := model.RequestRecord{
			ID:              "req-dominant-" + strconv.Itoa(index),
			RouteAlias:      "gpt-5.4",
			AccountID:       "provider-1",
			ProviderName:    "OpenAI",
			GatewayKeyID:    "gk-1",
			ClientSessionID: "sess-dominant",
			StatusCode:      200,
			StartedAt:       now.Add(-time.Duration(index) * time.Second),
			Usage:           model.UsageSummary{TotalTokens: 1},
		}
		if err := fileStore.AppendRequestRecord(record, nil, 100); err != nil {
			t.Fatalf("append dominant session record %d: %v", index, err)
		}
	}
	lateRecord := model.RequestRecord{
		ID:              "req-tail-session",
		RouteAlias:      "claude-3.7",
		AccountID:       "provider-2",
		ProviderName:    "Anthropic",
		GatewayKeyID:    "gk-2",
		ClientSessionID: "sess-tail",
		StatusCode:      200,
		StartedAt:       now.Add(-5 * time.Minute),
		Usage:           model.UsageSummary{TotalTokens: 10},
	}
	if err := fileStore.AppendRequestRecord(lateRecord, nil, 100); err != nil {
		t.Fatalf("append tail session record: %v", err)
	}

	items, err := fileStore.ActiveSessions(now, 15*time.Minute, 10)
	if err != nil {
		t.Fatalf("active sessions: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected both sessions to appear after full-window scan, got %+v", items)
	}
	if items[1].SessionID != "sess-tail" {
		t.Fatalf("expected tail session to remain visible, got %+v", items)
	}
}

func TestProviderUsageAggregatesRollingWindows(t *testing.T) {
	t.Parallel()

	fileStore, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	now := time.Now().UTC()
	for _, record := range []model.RequestRecord{
		{
			ID:           "req-1",
			AccountID:    "provider-1",
			ProviderName: "OpenAI",
			StatusCode:   200,
			StartedAt:    now.Add(-30 * time.Minute),
			Usage:        model.UsageSummary{InputTokens: 100, OutputTokens: 40, TotalTokens: 140},
			Billing:      model.BillingSummary{FinalCost: 1.25},
		},
		{
			ID:           "req-2",
			AccountID:    "provider-1",
			ProviderName: "OpenAI",
			StatusCode:   502,
			StartedAt:    now.Add(-2 * 24 * time.Hour),
			Usage:        model.UsageSummary{InputTokens: 60, OutputTokens: 20, TotalTokens: 80},
			Billing:      model.BillingSummary{FinalCost: 0.75},
		},
		{
			ID:           "req-3",
			AccountID:    "provider-2",
			ProviderName: "Anthropic",
			StatusCode:   200,
			StartedAt:    now.Add(-10 * 24 * time.Hour),
			Usage:        model.UsageSummary{InputTokens: 20, OutputTokens: 10, TotalTokens: 30},
			Billing:      model.BillingSummary{FinalCost: 0.20},
		},
	} {
		if err := fileStore.AppendRequestRecord(record, nil, 10); err != nil {
			t.Fatalf("append %s: %v", record.ID, err)
		}
	}

	items, err := fileStore.ProviderUsage(now, 10)
	if err != nil {
		t.Fatalf("provider usage: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected two provider rows, got %+v", items)
	}
	if items[0].ProviderID != "provider-1" {
		t.Fatalf("expected provider-1 first, got %+v", items)
	}
	if items[0].HourlyRequests != 1 || items[0].WeeklyRequests != 2 {
		t.Fatalf("unexpected request windows: %+v", items[0])
	}
	if items[0].MonthlyCostUSD != 2.0 || items[0].MonthlyErrors != 1 {
		t.Fatalf("unexpected monthly provider usage: %+v", items[0])
	}
}

func TestProviderUsageScansFullWindowInsteadOfSamplingRecentRows(t *testing.T) {
	t.Parallel()

	fileStore, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer fileStore.Close()

	now := time.Now().UTC()
	for index := 0; index < 120; index++ {
		record := model.RequestRecord{
			ID:           "req-provider-a-" + strconv.Itoa(index),
			AccountID:    "provider-a",
			ProviderName: "Provider A",
			StatusCode:   200,
			StartedAt:    now.Add(-time.Duration(index) * time.Minute),
			Billing:      model.BillingSummary{FinalCost: 0.01},
		}
		if err := fileStore.AppendRequestRecord(record, nil, 200); err != nil {
			t.Fatalf("append provider-a record %d: %v", index, err)
		}
	}
	if err := fileStore.AppendRequestRecord(model.RequestRecord{
		ID:           "req-provider-b-tail",
		AccountID:    "provider-b",
		ProviderName: "Provider B",
		StatusCode:   200,
		StartedAt:    now.Add(-20 * 24 * time.Hour),
		Billing:      model.BillingSummary{FinalCost: 0.25},
	}, nil, 200); err != nil {
		t.Fatalf("append provider-b tail record: %v", err)
	}

	items, err := fileStore.ProviderUsage(now, 10)
	if err != nil {
		t.Fatalf("provider usage: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected both providers to remain visible after full-window scan, got %+v", items)
	}
	if items[1].ProviderID != "provider-b" {
		t.Fatalf("expected provider-b to remain visible, got %+v", items)
	}
}

func boolPointer(value bool) *bool {
	v := value
	return &v
}
