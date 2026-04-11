package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const (
	defaultRequestHistoryPageSize = 50
	maxRequestHistoryPageSize     = 200
	defaultActiveSessionLimit     = 50
	maxActiveSessionLimit         = 200
	defaultProviderUsageLimit     = 200
	maxProviderUsageLimit         = 500
)

// RequestHistoryQuery describes request-history filters and cursor pagination.
type RequestHistoryQuery struct {
	Limit        int
	Cursor       string
	Protocol     model.Protocol
	RouteAlias   string
	ProviderID   string
	GatewayKeyID string
	SessionID    string
	StatusCode   *int
	Stream       *bool
	HasError     *bool
	Search       string
}

// RequestHistoryPage is the paginated request-history response.
type RequestHistoryPage struct {
	Items      []model.RequestRecord `json:"items"`
	HasMore    bool                  `json:"has_more"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// ActiveSessionSummary aggregates recent request history by client session ID.
type ActiveSessionSummary struct {
	SessionID       string    `json:"session_id"`
	GatewayKeyID    string    `json:"gateway_key_id,omitempty"`
	ProviderID      string    `json:"provider_id,omitempty"`
	ProviderName    string    `json:"provider_name,omitempty"`
	RouteAlias      string    `json:"route_alias,omitempty"`
	FirstRequestAt  time.Time `json:"first_request_at"`
	LastRequestAt   time.Time `json:"last_request_at"`
	RequestCount    int       `json:"request_count"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	FinalCostUSD    float64   `json:"final_cost_usd"`
	TotalDurationMS int64     `json:"total_duration_ms"`
	LastStatusCode  int       `json:"last_status_code"`
}

// ProviderUsageRow summarizes rolling usage windows per provider.
type ProviderUsageRow struct {
	ProviderID          string     `json:"provider_id,omitempty"`
	ProviderName        string     `json:"provider_name"`
	LastRequestAt       *time.Time `json:"last_request_at,omitempty"`
	HourlyRequests      int64      `json:"hourly_requests"`
	DailyRequests       int64      `json:"daily_requests"`
	WeeklyRequests      int64      `json:"weekly_requests"`
	MonthlyRequests     int64      `json:"monthly_requests"`
	HourlyCostUSD       float64    `json:"hourly_cost_usd"`
	DailyCostUSD        float64    `json:"daily_cost_usd"`
	WeeklyCostUSD       float64    `json:"weekly_cost_usd"`
	MonthlyCostUSD      float64    `json:"monthly_cost_usd"`
	MonthlyInputTokens  int64      `json:"monthly_input_tokens"`
	MonthlyOutputTokens int64      `json:"monthly_output_tokens"`
	MonthlyTotalTokens  int64      `json:"monthly_total_tokens"`
	MonthlyErrors       int64      `json:"monthly_errors"`
}

type requestHistoryCursor struct {
	StartedAt time.Time
	ID        string
}

type requestHistoryRow struct {
	Record model.RequestRecord
	Cursor requestHistoryCursor
}

// QueryRequestHistory returns paginated request history with optional filters.
func (s *FileStore) QueryRequestHistory(query RequestHistoryQuery) (RequestHistoryPage, error) {
	db, err := s.readDB()
	if err != nil {
		return RequestHistoryPage{}, err
	}

	limit := normalizeRequestHistoryLimit(query.Limit)
	cursor, err := decodeRequestHistoryCursor(query.Cursor)
	if err != nil {
		return RequestHistoryPage{}, err
	}
	fetchLimit := requestHistoryFetchLimit(limit, query)

	items := make([]model.RequestRecord, 0, limit+1)
	currentCursor := cursor
	for len(items) < limit+1 {
		batch, err := s.queryRequestHistoryBatch(db, query, currentCursor, fetchLimit)
		if err != nil {
			return RequestHistoryPage{}, err
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			if !requestHistoryMatches(row.Record, query) {
				continue
			}
			items = append(items, row.Record)
			if len(items) >= limit+1 {
				break
			}
		}
		if len(batch) < fetchLimit {
			break
		}
		currentCursor = batch[len(batch)-1].Cursor
	}

	page := RequestHistoryPage{
		Items:   items,
		HasMore: len(items) > limit,
	}
	if page.HasMore {
		page.Items = append([]model.RequestRecord(nil), page.Items[:limit]...)
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeRequestHistoryCursor(requestHistoryCursor{
			StartedAt: last.StartedAt.UTC(),
			ID:        last.ID,
		})
	}
	return page, nil
}

// RequestRecord returns one request-history record by its ID.
func (s *FileStore) RequestRecord(id string) (model.RequestRecord, bool, error) {
	db, err := s.readDB()
	if err != nil {
		return model.RequestRecord{}, false, err
	}

	row := db.QueryRow(s.rebind(`SELECT payload FROM request_records WHERE id = ?`), strings.TrimSpace(id))
	var raw []byte
	switch err := row.Scan(&raw); {
	case errors.Is(err, sql.ErrNoRows):
		return model.RequestRecord{}, false, nil
	case err != nil:
		return model.RequestRecord{}, false, err
	}

	var record model.RequestRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return model.RequestRecord{}, false, err
	}
	return record, true, nil
}

// ActiveSessions summarizes request history entries that fall within one recent
// time window.
func (s *FileStore) ActiveSessions(now time.Time, activeWithin time.Duration, limit int) ([]ActiveSessionSummary, error) {
	db, err := s.readDB()
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	if activeWithin <= 0 {
		activeWithin = 15 * time.Minute
	}
	limit = normalizeActiveSessionLimit(limit)
	cutoff := now.Add(-activeWithin)

	rows, err := db.Query(
		s.rebind(`SELECT payload FROM request_records WHERE started_at >= ? ORDER BY started_at DESC, id DESC`),
		formatStoreTime(cutoff),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type sessionAggregate struct {
		ActiveSessionSummary
	}
	bySession := map[string]*sessionAggregate{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record model.RequestRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		sessionID := strings.TrimSpace(record.ClientSessionID)
		if sessionID == "" {
			continue
		}
		current := bySession[sessionID]
		if current == nil {
			current = &sessionAggregate{
				ActiveSessionSummary: ActiveSessionSummary{
					SessionID:      sessionID,
					GatewayKeyID:   record.GatewayKeyID,
					ProviderID:     record.AccountID,
					ProviderName:   record.ProviderName,
					RouteAlias:     record.RouteAlias,
					FirstRequestAt: record.StartedAt,
					LastRequestAt:  record.StartedAt,
					LastStatusCode: record.StatusCode,
				},
			}
			bySession[sessionID] = current
		}
		if record.StartedAt.Before(current.FirstRequestAt) {
			current.FirstRequestAt = record.StartedAt
		}
		if record.StartedAt.After(current.LastRequestAt) {
			current.LastRequestAt = record.StartedAt
			current.LastStatusCode = record.StatusCode
			current.ProviderID = record.AccountID
			current.ProviderName = record.ProviderName
			current.RouteAlias = record.RouteAlias
			current.GatewayKeyID = record.GatewayKeyID
		}
		current.RequestCount++
		current.InputTokens += record.Usage.InputTokens
		current.OutputTokens += record.Usage.OutputTokens
		current.TotalTokens += record.Usage.TotalTokens
		current.ReasoningTokens += record.Usage.ReasoningTokens
		current.FinalCostUSD += record.Billing.FinalCost
		current.TotalDurationMS += record.DurationMS
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	items := make([]ActiveSessionSummary, 0, len(bySession))
	for _, current := range bySession {
		items = append(items, current.ActiveSessionSummary)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].LastRequestAt.Equal(items[j].LastRequestAt) {
			return items[i].LastRequestAt.After(items[j].LastRequestAt)
		}
		return items[i].SessionID < items[j].SessionID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// ProviderUsage returns rolling usage windows for providers seen in request
// history during the last 30 days.
func (s *FileStore) ProviderUsage(now time.Time, limit int) ([]ProviderUsageRow, error) {
	db, err := s.readDB()
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	limit = normalizeProviderUsageLimit(limit)
	cutoff := now.Add(-30 * 24 * time.Hour)

	rows, err := db.Query(
		s.rebind(`SELECT payload FROM request_records WHERE started_at >= ? ORDER BY started_at DESC, id DESC`),
		formatStoreTime(cutoff),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byProvider := map[string]*ProviderUsageRow{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record model.RequestRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		key := strings.TrimSpace(record.AccountID) + "\x00" + strings.TrimSpace(record.ProviderName)
		current := byProvider[key]
		if current == nil {
			current = &ProviderUsageRow{
				ProviderID:   strings.TrimSpace(record.AccountID),
				ProviderName: strings.TrimSpace(record.ProviderName),
			}
			byProvider[key] = current
		}
		recordAt := record.StartedAt.UTC()
		if current.LastRequestAt == nil || recordAt.After(*current.LastRequestAt) {
			next := recordAt
			current.LastRequestAt = &next
		}
		age := now.Sub(recordAt)
		if age <= time.Hour {
			current.HourlyRequests++
			current.HourlyCostUSD += record.Billing.FinalCost
		}
		if age <= 24*time.Hour {
			current.DailyRequests++
			current.DailyCostUSD += record.Billing.FinalCost
		}
		if age <= 7*24*time.Hour {
			current.WeeklyRequests++
			current.WeeklyCostUSD += record.Billing.FinalCost
		}
		if age <= 30*24*time.Hour {
			current.MonthlyRequests++
			current.MonthlyCostUSD += record.Billing.FinalCost
			current.MonthlyInputTokens += record.Usage.InputTokens
			current.MonthlyOutputTokens += record.Usage.OutputTokens
			current.MonthlyTotalTokens += record.Usage.TotalTokens
			if record.StatusCode >= 400 {
				current.MonthlyErrors++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	items := make([]ProviderUsageRow, 0, len(byProvider))
	for _, current := range byProvider {
		items = append(items, *current)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].MonthlyCostUSD != items[j].MonthlyCostUSD {
			return items[i].MonthlyCostUSD > items[j].MonthlyCostUSD
		}
		if items[i].MonthlyRequests != items[j].MonthlyRequests {
			return items[i].MonthlyRequests > items[j].MonthlyRequests
		}
		if items[i].ProviderName != items[j].ProviderName {
			return items[i].ProviderName < items[j].ProviderName
		}
		return items[i].ProviderID < items[j].ProviderID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *FileStore) queryRequestHistoryBatch(db *sql.DB, query RequestHistoryQuery, cursor requestHistoryCursor, limit int) ([]requestHistoryRow, error) {
	builder := strings.Builder{}
	builder.WriteString(`SELECT id, started_at, payload FROM request_records WHERE 1=1`)
	args := make([]any, 0, 12)

	if !cursor.StartedAt.IsZero() && strings.TrimSpace(cursor.ID) != "" {
		builder.WriteString(` AND (started_at < ? OR (started_at = ? AND id < ?))`)
		startedAt := formatStoreTime(cursor.StartedAt)
		args = append(args, startedAt, startedAt, cursor.ID)
	}
	if query.Protocol != "" {
		builder.WriteString(` AND protocol = ?`)
		args = append(args, query.Protocol)
	}
	if routeAlias := strings.TrimSpace(query.RouteAlias); routeAlias != "" {
		builder.WriteString(` AND route_alias = ?`)
		args = append(args, routeAlias)
	}
	if providerID := strings.TrimSpace(query.ProviderID); providerID != "" {
		builder.WriteString(` AND account_id = ?`)
		args = append(args, providerID)
	}
	if gatewayKeyID := strings.TrimSpace(query.GatewayKeyID); gatewayKeyID != "" {
		builder.WriteString(` AND gateway_key_id = ?`)
		args = append(args, gatewayKeyID)
	}
	if query.StatusCode != nil {
		builder.WriteString(` AND status_code = ?`)
		args = append(args, *query.StatusCode)
	}
	if query.Stream != nil {
		builder.WriteString(` AND stream = ?`)
		if *query.Stream {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	builder.WriteString(` ORDER BY started_at DESC, id DESC LIMIT ?`)
	args = append(args, limit)

	rows, err := db.Query(s.rebind(builder.String()), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]requestHistoryRow, 0, limit)
	for rows.Next() {
		var (
			id         string
			startedRaw string
			raw        []byte
		)
		if err := rows.Scan(&id, &startedRaw, &raw); err != nil {
			return nil, err
		}
		startedAt, err := parseStoreTime(startedRaw)
		if err != nil {
			return nil, err
		}
		var record model.RequestRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		items = append(items, requestHistoryRow{
			Record: record,
			Cursor: requestHistoryCursor{
				StartedAt: startedAt.UTC(),
				ID:        id,
			},
		})
	}
	return items, rows.Err()
}

func requestHistoryMatches(record model.RequestRecord, query RequestHistoryQuery) bool {
	if sessionID := strings.TrimSpace(query.SessionID); sessionID != "" && !strings.EqualFold(strings.TrimSpace(record.ClientSessionID), sessionID) {
		return false
	}
	if query.HasError != nil {
		hasError := strings.TrimSpace(record.Error) != ""
		if *query.HasError != hasError {
			return false
		}
	}
	search := strings.ToLower(strings.TrimSpace(query.Search))
	if search == "" {
		return true
	}
	haystacks := []string{
		record.ID,
		record.RouteAlias,
		record.ProviderName,
		record.AccountID,
		record.GatewayKeyID,
		record.ClientSessionID,
		record.UpstreamModel,
		record.Error,
		record.Path,
	}
	for _, haystack := range haystacks {
		if strings.Contains(strings.ToLower(haystack), search) {
			return true
		}
	}
	return false
}

func normalizeRequestHistoryLimit(limit int) int {
	if limit <= 0 {
		return defaultRequestHistoryPageSize
	}
	if limit > maxRequestHistoryPageSize {
		return maxRequestHistoryPageSize
	}
	return limit
}

func requestHistoryFetchLimit(limit int, query RequestHistoryQuery) int {
	fetch := limit + 1
	if strings.TrimSpace(query.SessionID) != "" || strings.TrimSpace(query.Search) != "" || query.HasError != nil {
		fetch *= 6
	}
	if fetch > 2000 {
		return 2000
	}
	return fetch
}

func normalizeActiveSessionLimit(limit int) int {
	if limit <= 0 {
		return defaultActiveSessionLimit
	}
	if limit > maxActiveSessionLimit {
		return maxActiveSessionLimit
	}
	return limit
}

func normalizeProviderUsageLimit(limit int) int {
	if limit <= 0 {
		return defaultProviderUsageLimit
	}
	if limit > maxProviderUsageLimit {
		return maxProviderUsageLimit
	}
	return limit
}

func encodeRequestHistoryCursor(cursor requestHistoryCursor) string {
	if cursor.StartedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
		return ""
	}
	value := strconv.FormatInt(cursor.StartedAt.UTC().UnixNano(), 10) + "\x00" + cursor.ID
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeRequestHistoryCursor(raw string) (requestHistoryCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return requestHistoryCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return requestHistoryCursor{}, fmt.Errorf("cursor must be valid base64url data")
	}
	parts := strings.SplitN(string(decoded), "\x00", 2)
	if len(parts) != 2 {
		return requestHistoryCursor{}, fmt.Errorf("cursor is invalid")
	}
	unixNS, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return requestHistoryCursor{}, fmt.Errorf("cursor timestamp is invalid")
	}
	id := strings.TrimSpace(parts[1])
	if id == "" {
		return requestHistoryCursor{}, fmt.Errorf("cursor request id is missing")
	}
	return requestHistoryCursor{
		StartedAt: time.Unix(0, unixNS).UTC(),
		ID:        id,
	}, nil
}

func (s *FileStore) readDB() (*sql.DB, error) {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return nil, errors.New("store is closed")
	}
	return db, nil
}
