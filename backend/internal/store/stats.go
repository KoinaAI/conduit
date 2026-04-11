package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const (
	defaultStatsWindowDays = 7
	maxStatsWindowDays     = 3650
)

// StatsWindow describes the trailing time range applied to request statistics.
type StatsWindow struct {
	Label   string    `json:"label"`
	Days    int       `json:"days"`
	StartAt time.Time `json:"start_at"`
	EndAt   time.Time `json:"end_at"`
}

// StatsMetrics contains aggregate request, usage, and billing counters.
type StatsMetrics struct {
	RequestCount      int64   `json:"request_count"`
	SuccessCount      int64   `json:"success_count"`
	ErrorCount        int64   `json:"error_count"`
	StreamCount       int64   `json:"stream_count"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	FinalCost         float64 `json:"final_cost"`
	ErrorRate         float64 `json:"error_rate"`
}

// StatsSummaryReport is the top-level payload for aggregate request statistics.
type StatsSummaryReport struct {
	Window  StatsWindow  `json:"window"`
	Summary StatsMetrics `json:"summary"`
}

// StatsBreakdownRow represents one grouped statistics row.
type StatsBreakdownRow struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	StatsMetrics
}

// StatsBreakdownReport is the response envelope for grouped statistics.
type StatsBreakdownReport struct {
	Window StatsWindow         `json:"window"`
	Items  []StatsBreakdownRow `json:"items"`
}

// ResolveStatsWindow parses admin stats window parameters using the provided
// reference time so tests can stay deterministic.
func ResolveStatsWindow(windowValue, daysValue string, now time.Time) (StatsWindow, error) {
	now = now.UTC()
	windowValue = strings.ToLower(strings.TrimSpace(windowValue))
	daysValue = strings.TrimSpace(daysValue)
	if windowValue != "" && daysValue != "" {
		return StatsWindow{}, errors.New("window and days cannot be combined")
	}
	if windowValue == "" && daysValue == "" {
		windowValue = fmt.Sprintf("%dd", defaultStatsWindowDays)
	}

	switch windowValue {
	case "today":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return StatsWindow{
			Label:   "today",
			Days:    1,
			StartAt: start,
			EndAt:   now,
		}, nil
	case "7d", "30d":
		parsedDays, _ := strconv.Atoi(strings.TrimSuffix(windowValue, "d"))
		return statsWindowForDays(parsedDays, now)
	case "":
		parsedDays, err := strconv.Atoi(daysValue)
		if err != nil {
			return StatsWindow{}, errors.New("days must be a positive integer")
		}
		return statsWindowForDays(parsedDays, now)
	default:
		return StatsWindow{}, errors.New("window must be one of today, 7d, or 30d")
	}
}

// StatsSummary returns aggregate statistics for one time window.
func (s *FileStore) StatsSummary(window StatsWindow) (StatsSummaryReport, error) {
	var metrics StatsMetrics
	if err := s.scanStatsRows(window, func(record statsRecord) error {
		metrics.addRecord(record)
		return nil
	}); err != nil {
		return StatsSummaryReport{}, err
	}
	metrics.finalize()
	return StatsSummaryReport{
		Window:  window,
		Summary: metrics,
	}, nil
}

// StatsByGatewayKey returns grouped statistics for gateway keys.
func (s *FileStore) StatsByGatewayKey(window StatsWindow) (StatsBreakdownReport, error) {
	nameByID := s.gatewayKeyNameMap()
	return s.statsBreakdown(window, func(record model.RequestRecord) (string, string) {
		if record.GatewayKeyID == "" {
			return "", "anonymous"
		}
		if name := nameByID[record.GatewayKeyID]; strings.TrimSpace(name) != "" {
			return record.GatewayKeyID, name
		}
		return record.GatewayKeyID, record.GatewayKeyID
	})
}

// StatsByProvider returns grouped statistics for providers.
func (s *FileStore) StatsByProvider(window StatsWindow) (StatsBreakdownReport, error) {
	return s.statsBreakdown(window, func(record model.RequestRecord) (string, string) {
		id := strings.TrimSpace(record.AccountID)
		name := strings.TrimSpace(record.ProviderName)
		switch {
		case name != "":
			return id, name
		case id != "":
			return id, id
		default:
			return "", "unknown"
		}
	})
}

// StatsByModel returns grouped statistics for routed model aliases.
func (s *FileStore) StatsByModel(window StatsWindow) (StatsBreakdownReport, error) {
	return s.statsBreakdown(window, func(record model.RequestRecord) (string, string) {
		name := strings.TrimSpace(record.RouteAlias)
		if name == "" {
			name = strings.TrimSpace(record.UpstreamModel)
		}
		if name == "" {
			name = "unknown"
		}
		return name, name
	})
}

func statsWindowForDays(days int, now time.Time) (StatsWindow, error) {
	if days <= 0 || days > maxStatsWindowDays {
		return StatsWindow{}, fmt.Errorf("days must be between 1 and %d", maxStatsWindowDays)
	}
	return StatsWindow{
		Label:   fmt.Sprintf("%dd", days),
		Days:    days,
		StartAt: now.Add(-time.Duration(days) * 24 * time.Hour),
		EndAt:   now,
	}, nil
}

func (s *FileStore) statsBreakdown(window StatsWindow, groupKey func(model.RequestRecord) (string, string)) (StatsBreakdownReport, error) {
	type group struct {
		id      string
		name    string
		metrics StatsMetrics
	}

	groups := map[string]*group{}
	if err := s.scanStatsRows(window, func(record statsRecord) error {
		id, name := groupKey(record.RequestRecord())
		compositeKey := id + "\x00" + name
		current, ok := groups[compositeKey]
		if !ok {
			current = &group{id: id, name: name}
			groups[compositeKey] = current
		}
		current.metrics.addRecord(record)
		return nil
	}); err != nil {
		return StatsBreakdownReport{}, err
	}

	items := make([]StatsBreakdownRow, 0, len(groups))
	for _, current := range groups {
		current.metrics.finalize()
		items = append(items, StatsBreakdownRow{
			ID:           current.id,
			Name:         current.name,
			StatsMetrics: current.metrics,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].RequestCount != items[j].RequestCount {
			return items[i].RequestCount > items[j].RequestCount
		}
		if items[i].FinalCost != items[j].FinalCost {
			return items[i].FinalCost > items[j].FinalCost
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].ID < items[j].ID
	})

	return StatsBreakdownReport{
		Window: window,
		Items:  items,
	}, nil
}

type statsRecord struct {
	routeAlias    string
	gatewayKeyID  string
	accountID     string
	providerName  string
	upstreamModel string
	statusCode    int
	stream        bool
	usage         model.UsageSummary
	billing       model.BillingSummary
}

func (r statsRecord) RequestRecord() model.RequestRecord {
	return model.RequestRecord{
		RouteAlias:    r.routeAlias,
		GatewayKeyID:  r.gatewayKeyID,
		AccountID:     r.accountID,
		ProviderName:  r.providerName,
		UpstreamModel: r.upstreamModel,
		StatusCode:    r.statusCode,
		Stream:        r.stream,
		Usage:         r.usage,
		Billing:       r.billing,
	}
}

func (s *FileStore) scanStatsRows(window StatsWindow, visit func(statsRecord) error) error {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return errors.New("store is closed")
	}

	rows, err := db.Query(
		s.rebind(`SELECT route_alias, gateway_key_id, account_id, provider_name, status_code, stream, payload FROM request_records WHERE started_at >= ? AND started_at <= ? ORDER BY started_at DESC`),
		formatStoreTime(window.StartAt),
		formatStoreTime(window.EndAt),
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		record, err := scanStatsRecord(rows)
		if err != nil {
			return err
		}
		if err := visit(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanStatsRecord(rows *sql.Rows) (statsRecord, error) {
	var record statsRecord
	var raw []byte
	var stream int
	if err := rows.Scan(&record.routeAlias, &record.gatewayKeyID, &record.accountID, &record.providerName, &record.statusCode, &stream, &raw); err != nil {
		return statsRecord{}, err
	}
	var payload struct {
		UpstreamModel string               `json:"upstream_model"`
		Usage         model.UsageSummary   `json:"usage"`
		Billing       model.BillingSummary `json:"billing"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return statsRecord{}, err
	}
	record.upstreamModel = payload.UpstreamModel
	record.stream = stream != 0
	record.usage = payload.Usage
	record.billing = payload.Billing
	return record, nil
}

func (s *FileStore) gatewayKeyNameMap() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make(map[string]string, len(s.state.GatewayKeys))
	for _, key := range s.state.GatewayKeys {
		names[key.ID] = key.Name
	}
	return names
}

func (m *StatsMetrics) addRecord(record statsRecord) {
	m.RequestCount++
	if statsStatusCountsAsSuccess(record.statusCode) {
		m.SuccessCount++
	} else {
		m.ErrorCount++
	}
	if record.stream {
		m.StreamCount++
	}
	m.InputTokens += record.usage.InputTokens
	m.OutputTokens += record.usage.OutputTokens
	m.TotalTokens += record.usage.TotalTokens
	m.CachedInputTokens += record.usage.CachedInputTokens
	m.ReasoningTokens += record.usage.ReasoningTokens
	m.FinalCost += record.billing.FinalCost
}

func (m *StatsMetrics) finalize() {
	if m.RequestCount > 0 {
		m.ErrorRate = roundStat(float64(m.ErrorCount) / float64(m.RequestCount))
	}
	m.FinalCost = roundStat(m.FinalCost)
}

func roundStat(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

func statsStatusCountsAsSuccess(statusCode int) bool {
	return statusCode == httpStatusSwitchingProtocols ||
		(statusCode >= httpStatusSuccessMin && statusCode < httpStatusSuccessMax)
}

const (
	httpStatusSwitchingProtocols = 101
	httpStatusSuccessMin         = 200
	httpStatusSuccessMax         = 400
)
