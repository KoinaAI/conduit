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
	records, err := s.statsRecords(window)
	if err != nil {
		return StatsSummaryReport{}, err
	}
	var metrics StatsMetrics
	for _, record := range records {
		metrics.addRecord(record)
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
	records, err := s.statsRecords(window)
	if err != nil {
		return StatsBreakdownReport{}, err
	}

	type group struct {
		id      string
		name    string
		metrics StatsMetrics
	}

	groups := map[string]*group{}
	for _, record := range records {
		id, name := groupKey(record)
		compositeKey := id + "\x00" + name
		current, ok := groups[compositeKey]
		if !ok {
			current = &group{id: id, name: name}
			groups[compositeKey] = current
		}
		current.metrics.addRecord(record)
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

func (s *FileStore) statsRecords(window StatsWindow) ([]model.RequestRecord, error) {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return nil, errors.New("store is closed")
	}

	rows, err := db.Query(
		`SELECT payload FROM request_records WHERE started_at >= ? AND started_at <= ? ORDER BY started_at DESC`,
		window.StartAt.UTC().Format(time.RFC3339Nano),
		window.EndAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []model.RequestRecord{}
	for rows.Next() {
		record, err := scanRequestRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func scanRequestRecord(rows *sql.Rows) (model.RequestRecord, error) {
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return model.RequestRecord{}, err
	}
	var record model.RequestRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return model.RequestRecord{}, err
	}
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

func (m *StatsMetrics) addRecord(record model.RequestRecord) {
	m.RequestCount++
	if record.StatusCode >= httpStatusSuccessMin && record.StatusCode < httpStatusSuccessMax {
		m.SuccessCount++
	} else {
		m.ErrorCount++
	}
	if record.Stream {
		m.StreamCount++
	}
	m.InputTokens += record.Usage.InputTokens
	m.OutputTokens += record.Usage.OutputTokens
	m.TotalTokens += record.Usage.TotalTokens
	m.CachedInputTokens += record.Usage.CachedInputTokens
	m.ReasoningTokens += record.Usage.ReasoningTokens
	m.FinalCost += record.Billing.FinalCost
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

const (
	httpStatusSuccessMin = 200
	httpStatusSuccessMax = 400
)
