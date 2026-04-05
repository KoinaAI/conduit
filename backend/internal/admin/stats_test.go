package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/integration"
	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestStatsHandlersServeSummaryAndBreakdowns(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	state := model.DefaultState()
	state.GatewayKeys = []model.GatewayKey{
		{ID: "gk-1", Name: "Primary", Enabled: true},
		{ID: "gk-2", Name: "Fallback", Enabled: true},
	}
	state.RequestHistory = []model.RequestRecord{
		{
			ID:           "req-1",
			RouteAlias:   "gpt-5.4",
			AccountID:    "provider-1",
			ProviderName: "OpenAI",
			GatewayKeyID: "gk-1",
			StatusCode:   http.StatusOK,
			StartedAt:    now.Add(-2 * time.Hour),
			Usage:        model.UsageSummary{InputTokens: 100, OutputTokens: 60, TotalTokens: 160},
			Billing:      model.BillingSummary{Currency: "USD", FinalCost: 1.2},
		},
		{
			ID:           "req-2",
			RouteAlias:   "gpt-5.4",
			AccountID:    "provider-1",
			ProviderName: "OpenAI",
			GatewayKeyID: "gk-1",
			StatusCode:   http.StatusBadGateway,
			StartedAt:    now.Add(-24 * time.Hour),
			Usage:        model.UsageSummary{InputTokens: 20, OutputTokens: 0, TotalTokens: 20},
			Billing:      model.BillingSummary{Currency: "USD", FinalCost: 0.3},
		},
		{
			ID:           "req-3",
			RouteAlias:   "claude-3.7",
			AccountID:    "provider-2",
			ProviderName: "Anthropic",
			GatewayKeyID: "gk-2",
			StatusCode:   http.StatusOK,
			StartedAt:    now.Add(-3 * 24 * time.Hour),
			Usage:        model.UsageSummary{InputTokens: 10, OutputTokens: 40, TotalTokens: 50},
			Billing:      model.BillingSummary{Currency: "USD", FinalCost: 0.6},
		},
		{
			ID:           "req-4",
			RouteAlias:   "old-model",
			AccountID:    "provider-1",
			ProviderName: "OpenAI",
			GatewayKeyID: "gk-1",
			StatusCode:   http.StatusOK,
			StartedAt:    now.Add(-45 * 24 * time.Hour),
			Usage:        model.UsageSummary{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
			Billing:      model.BillingSummary{Currency: "USD", FinalCost: 0.01},
		},
	}
	fileStore := openTestStore(t, state)
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	t.Run("summary", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats/summary?days=7", nil)
		recorder := httptest.NewRecorder()
		handlers.GetStatsSummary(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
		}

		var payload struct {
			Window struct {
				Label string `json:"label"`
				Days  int    `json:"days"`
			} `json:"window"`
			Summary struct {
				RequestCount int64   `json:"request_count"`
				SuccessCount int64   `json:"success_count"`
				ErrorCount   int64   `json:"error_count"`
				TotalTokens  int64   `json:"total_tokens"`
				FinalCost    float64 `json:"final_cost"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode summary: %v", err)
		}
		if payload.Window.Label != "7d" || payload.Window.Days != 7 {
			t.Fatalf("unexpected window payload: %+v", payload.Window)
		}
		if payload.Summary.RequestCount != 3 || payload.Summary.SuccessCount != 2 || payload.Summary.ErrorCount != 1 {
			t.Fatalf("unexpected summary counts: %+v", payload.Summary)
		}
		if payload.Summary.TotalTokens != 230 || payload.Summary.FinalCost != 2.1 {
			t.Fatalf("unexpected summary totals: %+v", payload.Summary)
		}
	})

	t.Run("by key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats/by-key?days=7", nil)
		recorder := httptest.NewRecorder()
		handlers.GetStatsByGatewayKey(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
		}

		var payload struct {
			Items []struct {
				ID           string  `json:"id"`
				Name         string  `json:"name"`
				RequestCount int64   `json:"request_count"`
				FinalCost    float64 `json:"final_cost"`
			} `json:"items"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode by-key payload: %v", err)
		}
		if len(payload.Items) != 2 {
			t.Fatalf("expected 2 key rows, got %+v", payload.Items)
		}
		if payload.Items[0].ID != "gk-1" || payload.Items[0].Name != "Primary" || payload.Items[0].RequestCount != 2 {
			t.Fatalf("unexpected first key row: %+v", payload.Items[0])
		}
	})

	t.Run("by provider", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats/by-provider?days=7", nil)
		recorder := httptest.NewRecorder()
		handlers.GetStatsByProvider(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
		}

		var payload struct {
			Items []struct {
				ID           string `json:"id"`
				Name         string `json:"name"`
				RequestCount int64  `json:"request_count"`
			} `json:"items"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode by-provider payload: %v", err)
		}
		if len(payload.Items) != 2 || payload.Items[0].ID != "provider-1" || payload.Items[0].Name != "OpenAI" {
			t.Fatalf("unexpected provider rows: %+v", payload.Items)
		}
	})

	t.Run("by model", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats/by-model?days=7", nil)
		recorder := httptest.NewRecorder()
		handlers.GetStatsByModel(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
		}

		var payload struct {
			Items []struct {
				ID           string `json:"id"`
				RequestCount int64  `json:"request_count"`
				ErrorCount   int64  `json:"error_count"`
			} `json:"items"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode by-model payload: %v", err)
		}
		if len(payload.Items) != 2 {
			t.Fatalf("expected 2 model rows, got %+v", payload.Items)
		}
		if payload.Items[0].ID != "gpt-5.4" || payload.Items[0].RequestCount != 2 || payload.Items[0].ErrorCount != 1 {
			t.Fatalf("unexpected model rows: %+v", payload.Items)
		}
	})
}

func TestStatsHandlersValidateWindowParams(t *testing.T) {
	t.Parallel()

	fileStore := openTestStore(t, model.DefaultState())
	handlers := New(config.Config{}, fileStore, integration.NewService(integration.WithAllowPrivateBaseURLForTests()))

	t.Run("reject invalid window", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats/summary?window=quarter", nil)
		recorder := httptest.NewRecorder()
		handlers.GetStatsSummary(recorder, req)

		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("expected bad request, got %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("accept today window", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/stats/summary?window=today", nil)
		recorder := httptest.NewRecorder()
		handlers.GetStatsSummary(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected ok, got %d body=%s", recorder.Code, recorder.Body.String())
		}

		var payload struct {
			Window struct {
				Label string `json:"label"`
				Days  int    `json:"days"`
			} `json:"window"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode today payload: %v", err)
		}
		if payload.Window.Label != "today" || payload.Window.Days != 1 {
			t.Fatalf("unexpected today window payload: %+v", payload.Window)
		}
	})
}
