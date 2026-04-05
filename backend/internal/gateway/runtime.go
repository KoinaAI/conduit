package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const (
	defaultStickySweepInterval  = 64
	defaultRetryAfterCooldown   = 2 * time.Minute
	maxRetryAfterCooldown       = 15 * time.Minute
	defaultAuthFailureWindow    = 5 * time.Minute
	defaultAuthLockDuration     = 10 * time.Minute
	defaultAuthFailureLimit     = 20
	defaultInvalidLookupTTL     = 60 * time.Second
	defaultAuthSweepInterval    = 64
	defaultRuntimeSweepInterval = 128
	maxEndpointRuntimeAge       = 6 * time.Hour
	maxCredentialRuntimeAge     = 6 * time.Hour
	maxAuthRuntimeEntries       = 4096
)

type endpointRuntimeState struct {
	LastLatencyMS       int64
	ConsecutiveFailures int
	OpenUntil           time.Time
	HalfOpenInFlight    int
	LastStatusCode      int
	LastError           string
	LastCheckedAt       time.Time
}

type credentialRuntimeState struct {
	DisabledUntil       time.Time
	PermanentlyDisabled bool
	ConsecutiveFailures int
	LastStatusCode      int
	LastError           string
	LastCheckedAt       time.Time
}

type stickyBinding struct {
	ProviderID   string
	EndpointID   string
	CredentialID string
	ExpiresAt    time.Time
}

type gatewaySpendEvent struct {
	At      time.Time
	CostUSD float64
}

type gatewayKeyWindow struct {
	MinuteBucket time.Time
	RequestCount int
	SpendEvents  []gatewaySpendEvent
	InFlight     int
}

type invalidGatewayLookupState struct {
	CandidateFingerprint string
	ExpiresAt            time.Time
}

type gatewayAuthFailureState struct {
	WindowStartedAt time.Time
	LastFailedAt    time.Time
	FailureCount    int
	LockedUntil     time.Time
}

type runtimeState struct {
	mu                    sync.Mutex
	endpoints             map[string]*endpointRuntimeState
	credentials           map[string]*credentialRuntimeState
	sticky                map[string]stickyBinding
	sessions              map[string]LiveSessionStatus
	stickyStore           stickyBindingStore
	stickyWrites          int
	providerWindows       map[string]*gatewayKeyWindow
	keyWindows            map[string]*gatewayKeyWindow
	roundRobin            map[string]uint64
	invalidGatewayLookups map[string]invalidGatewayLookupState
	authFailures          map[string]*gatewayAuthFailureState
	authWrites            int
	runtimeWrites         int
}

type EndpointCircuitStatus struct {
	ProviderID          string     `json:"provider_id"`
	ProviderName        string     `json:"provider_name"`
	EndpointID          string     `json:"endpoint_id"`
	EndpointURL         string     `json:"endpoint_url"`
	State               string     `json:"state"`
	Enabled             bool       `json:"enabled"`
	FailureThreshold    int        `json:"failure_threshold"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	HalfOpenInFlight    int        `json:"half_open_inflight"`
	OpenUntil           *time.Time `json:"open_until,omitempty"`
	LastCheckedAt       *time.Time `json:"last_checked_at,omitempty"`
	LastStatusCode      int        `json:"last_status_code,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

type StickyBindingStatus struct {
	GatewayKeyID string    `json:"gateway_key_id"`
	RouteAlias   string    `json:"route_alias"`
	Scenario     string    `json:"scenario,omitempty"`
	SessionID    string    `json:"session_id"`
	ProviderID   string    `json:"provider_id"`
	EndpointID   string    `json:"endpoint_id"`
	CredentialID string    `json:"credential_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type LiveSessionStatus struct {
	RequestID    string         `json:"request_id"`
	SessionID    string         `json:"session_id"`
	GatewayKeyID string         `json:"gateway_key_id,omitempty"`
	ProviderID   string         `json:"provider_id,omitempty"`
	ProviderName string         `json:"provider_name,omitempty"`
	EndpointID   string         `json:"endpoint_id,omitempty"`
	CredentialID string         `json:"credential_id,omitempty"`
	RouteAlias   string         `json:"route_alias,omitempty"`
	Scenario     string         `json:"scenario,omitempty"`
	Protocol     model.Protocol `json:"protocol"`
	Transport    string         `json:"transport"`
	Path         string         `json:"path,omitempty"`
	Stream       bool           `json:"stream,omitempty"`
	StartedAt    time.Time      `json:"started_at"`
	LastSeenAt   time.Time      `json:"last_seen_at"`
	ExpiresAt    time.Time      `json:"expires_at"`
}

type ProviderRuntimeStatus struct {
	ProviderID            string    `json:"provider_id"`
	ProviderName          string    `json:"provider_name"`
	RateLimitRPM          int       `json:"rate_limit_rpm,omitempty"`
	MaxConcurrency        int       `json:"max_concurrency,omitempty"`
	HourlyBudgetUSD       float64   `json:"hourly_budget_usd,omitempty"`
	DailyBudgetUSD        float64   `json:"daily_budget_usd,omitempty"`
	WeeklyBudgetUSD       float64   `json:"weekly_budget_usd,omitempty"`
	MonthlyBudgetUSD      float64   `json:"monthly_budget_usd,omitempty"`
	CurrentMinuteRequests int       `json:"current_minute_requests"`
	InFlight              int       `json:"in_flight"`
	HourlyCostUSD         float64   `json:"hourly_cost_usd"`
	DailyCostUSD          float64   `json:"daily_cost_usd"`
	WeeklyCostUSD         float64   `json:"weekly_cost_usd"`
	MonthlyCostUSD        float64   `json:"monthly_cost_usd"`
	MinuteBucketStartedAt time.Time `json:"minute_bucket_started_at,omitempty"`
}

func newRuntimeState(stickyStore ...stickyBindingStore) *runtimeState {
	var currentStickyStore stickyBindingStore
	if len(stickyStore) > 0 {
		currentStickyStore = stickyStore[0]
	}
	return &runtimeState{
		endpoints:             map[string]*endpointRuntimeState{},
		credentials:           map[string]*credentialRuntimeState{},
		sticky:                map[string]stickyBinding{},
		sessions:              map[string]LiveSessionStatus{},
		stickyStore:           currentStickyStore,
		providerWindows:       map[string]*gatewayKeyWindow{},
		keyWindows:            map[string]*gatewayKeyWindow{},
		roundRobin:            map[string]uint64{},
		invalidGatewayLookups: map[string]invalidGatewayLookupState{},
		authFailures:          map[string]*gatewayAuthFailureState{},
	}
}

func (r *runtimeState) ProviderUsage(state model.RoutingState, now time.Time, limit int) []ProviderRuntimeStatus {
	now = now.UTC()
	if limit <= 0 {
		limit = 200
	}
	items := make([]ProviderRuntimeStatus, 0, len(state.Providers))
	for _, provider := range state.Providers {
		var (
			status ProviderRuntimeStatus
			active bool
			loaded bool
		)
		if store, ok := r.stickyStore.(providerRuntimeStore); ok && store != nil {
			remote, remoteActive, err := store.LoadProviderRuntime(provider, now)
			if err != nil {
				slog.Warn("provider runtime list failed", "provider_id", provider.ID, "error", err)
			} else {
				status = remote
				active = remoteActive
				loaded = true
			}
		}
		if !loaded {
			status, active = r.localProviderUsage(provider, now)
		}
		if !active {
			continue
		}
		items = append(items, status)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].InFlight != items[j].InFlight {
			return items[i].InFlight > items[j].InFlight
		}
		if items[i].CurrentMinuteRequests != items[j].CurrentMinuteRequests {
			return items[i].CurrentMinuteRequests > items[j].CurrentMinuteRequests
		}
		if items[i].MonthlyCostUSD != items[j].MonthlyCostUSD {
			return items[i].MonthlyCostUSD > items[j].MonthlyCostUSD
		}
		if items[i].ProviderName != items[j].ProviderName {
			return items[i].ProviderName < items[j].ProviderName
		}
		return items[i].ProviderID < items[j].ProviderID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (r *runtimeState) localProviderUsage(provider model.Provider, now time.Time) (ProviderRuntimeStatus, bool) {
	r.mu.Lock()
	window := r.providerWindows[provider.ID]
	if window == nil {
		r.mu.Unlock()
		return ProviderRuntimeStatus{}, false
	}
	window.SpendEvents = pruneGatewaySpendEvents(window.SpendEvents, now)
	hourlyCost, dailyCost, weeklyCost, monthlyCost := gatewaySpendTotals(window.SpendEvents, now)
	status := ProviderRuntimeStatus{
		ProviderID:            provider.ID,
		ProviderName:          provider.Name,
		RateLimitRPM:          provider.RateLimitRPM,
		MaxConcurrency:        provider.MaxConcurrency,
		HourlyBudgetUSD:       provider.HourlyBudgetUSD,
		DailyBudgetUSD:        provider.DailyBudgetUSD,
		WeeklyBudgetUSD:       provider.WeeklyBudgetUSD,
		MonthlyBudgetUSD:      provider.MonthlyBudgetUSD,
		CurrentMinuteRequests: window.RequestCount,
		InFlight:              window.InFlight,
		HourlyCostUSD:         hourlyCost,
		DailyCostUSD:          dailyCost,
		WeeklyCostUSD:         weeklyCost,
		MonthlyCostUSD:        monthlyCost,
		MinuteBucketStartedAt: window.MinuteBucket,
	}
	r.mu.Unlock()
	return status, providerRuntimeActive(status)
}

func providerRuntimeActive(status ProviderRuntimeStatus) bool {
	return status.CurrentMinuteRequests > 0 ||
		status.InFlight > 0 ||
		status.HourlyCostUSD > 0 ||
		status.DailyCostUSD > 0 ||
		status.WeeklyCostUSD > 0 ||
		status.MonthlyCostUSD > 0
}

func (r *runtimeState) ActiveSessions(now time.Time, activeWithin time.Duration, limit int) []LiveSessionStatus {
	now = now.UTC()
	if activeWithin <= 0 {
		activeWithin = 15 * time.Minute
	}
	if limit <= 0 {
		limit = 50
	}
	cutoff := now.Add(-activeWithin)
	if store, ok := r.stickyStore.(sessionRuntimeStore); ok && store != nil {
		records, err := store.ListRuntimeSessions(now)
		if err != nil {
			slog.Warn("gateway live session list failed", "error", err)
		} else {
			return filterLiveSessions(runtimeSessionStatuses(records), cutoff, limit)
		}
	}

	r.mu.Lock()
	r.sweepExpiredSessionsLocked(now)
	items := make([]LiveSessionStatus, 0, len(r.sessions))
	for _, session := range r.sessions {
		items = append(items, session)
	}
	r.mu.Unlock()
	return filterLiveSessions(items, cutoff, limit)
}

func (r *runtimeState) StickyBindings(now time.Time) []StickyBindingStatus {
	if lister, ok := r.stickyStore.(stickyBindingListingStore); ok && lister != nil {
		remote, err := lister.ListStickyBindings(now)
		if err != nil {
			slog.Warn("gateway sticky binding list failed", "error", err)
		} else {
			return stickyBindingStatusesFromRecords(remote)
		}
	}
	r.mu.Lock()
	r.sweepExpiredStickyLocked(now)
	records := make([]stickyBindingRecord, 0, len(r.sticky))
	for key, binding := range r.sticky {
		records = append(records, stickyBindingRecord{Key: key, Binding: binding})
	}
	r.mu.Unlock()
	return stickyBindingStatusesFromRecords(records)
}

func stickyBindingStatusesFromRecords(records []stickyBindingRecord) []StickyBindingStatus {
	merged := make(map[string]StickyBindingStatus, len(records))
	for _, record := range records {
		status, ok := stickyBindingStatusFromRecord(record)
		if !ok {
			continue
		}
		merged[record.Key] = status
	}

	items := make([]StickyBindingStatus, 0, len(merged))
	for _, status := range merged {
		items = append(items, status)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].ExpiresAt.Equal(items[j].ExpiresAt) {
			return items[i].ExpiresAt.After(items[j].ExpiresAt)
		}
		if items[i].GatewayKeyID != items[j].GatewayKeyID {
			return items[i].GatewayKeyID < items[j].GatewayKeyID
		}
		if items[i].RouteAlias != items[j].RouteAlias {
			return items[i].RouteAlias < items[j].RouteAlias
		}
		if items[i].Scenario != items[j].Scenario {
			return items[i].Scenario < items[j].Scenario
		}
		return items[i].SessionID < items[j].SessionID
	})
	return items
}

func runtimeSessionStatuses(records []runtimeSessionRecord) []LiveSessionStatus {
	items := make([]LiveSessionStatus, 0, len(records))
	for _, record := range records {
		items = append(items, record.Session)
	}
	return items
}

func filterLiveSessions(items []LiveSessionStatus, cutoff time.Time, limit int) []LiveSessionStatus {
	filtered := make([]LiveSessionStatus, 0, len(items))
	for _, item := range items {
		if item.RequestID == "" || item.SessionID == "" {
			continue
		}
		if item.LastSeenAt.Before(cutoff) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].LastSeenAt.Equal(filtered[j].LastSeenAt) {
			return filtered[i].LastSeenAt.After(filtered[j].LastSeenAt)
		}
		if !filtered[i].StartedAt.Equal(filtered[j].StartedAt) {
			return filtered[i].StartedAt.After(filtered[j].StartedAt)
		}
		return filtered[i].RequestID < filtered[j].RequestID
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered
}

func (r *runtimeState) ResetStickyBindings(gatewayKeyID, routeAlias, scenario, sessionID string, now time.Time) int {
	keysToDelete := map[string]struct{}{}

	r.mu.Lock()
	r.sweepExpiredStickyLocked(now)
	for key, binding := range r.sticky {
		record := stickyBindingRecord{Key: key, Binding: binding}
		if stickyBindingMatches(record, gatewayKeyID, routeAlias, scenario, sessionID) {
			delete(r.sticky, key)
			keysToDelete[key] = struct{}{}
		}
	}
	r.mu.Unlock()

	if lister, ok := r.stickyStore.(stickyBindingListingStore); ok && lister != nil {
		records, err := lister.ListStickyBindings(now)
		if err != nil {
			slog.Warn("gateway sticky binding list failed during reset", "error", err)
		} else {
			for _, record := range records {
				if stickyBindingMatches(record, gatewayKeyID, routeAlias, scenario, sessionID) {
					keysToDelete[record.Key] = struct{}{}
				}
			}
		}
	}

	if r.stickyStore != nil {
		for key := range keysToDelete {
			if err := r.stickyStore.DeleteStickyBinding(key); err != nil {
				slog.Warn("gateway sticky binding delete failed", "key", key, "error", err)
			}
		}
	}
	return len(keysToDelete)
}

func (r *runtimeState) endpointState(key string) *endpointRuntimeState {
	state := r.endpoints[key]
	if state == nil {
		state = &endpointRuntimeState{}
		r.endpoints[key] = state
	}
	return state
}

func (r *runtimeState) credentialState(key string) *credentialRuntimeState {
	state := r.credentials[key]
	if state == nil {
		state = &credentialRuntimeState{}
		r.credentials[key] = state
	}
	return state
}

func (r *runtimeState) endpointLatency(candidate resolvedCandidate) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state := r.endpoints[endpointRuntimeKey(candidate)]; state != nil {
		return state.LastLatencyMS
	}
	return 0
}

func (r *runtimeState) endpointOpen(candidate resolvedCandidate, now time.Time) bool {
	if store, ok := r.stickyStore.(endpointRuntimeStore); ok && store != nil {
		open, err := store.EndpointOpen(candidate, now)
		if err == nil {
			return open
		}
		slog.Warn("endpoint runtime cache read failed", "provider_id", candidate.provider.ID, "endpoint_id", candidate.endpoint.ID, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endpointOpenLocked(candidate, now)
}

func (r *runtimeState) endpointStatus(candidate resolvedCandidate, now time.Time) (endpointRuntimeState, bool) {
	if store, ok := r.stickyStore.(endpointRuntimeStore); ok && store != nil {
		state, exists, err := store.LoadEndpointState(candidate)
		if err == nil {
			return state, exists
		}
		slog.Warn("endpoint runtime cache state read failed", "provider_id", candidate.provider.ID, "endpoint_id", candidate.endpoint.ID, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.endpoints[endpointRuntimeKey(candidate)]
	if !ok || state == nil {
		return endpointRuntimeState{}, false
	}
	return *state, true
}

func (r *runtimeState) CircuitStatuses(state model.RoutingState, now time.Time) []EndpointCircuitStatus {
	now = now.UTC()
	items := make([]EndpointCircuitStatus, 0)
	for _, provider := range state.Providers {
		for _, endpoint := range provider.Endpoints {
			if strings.TrimSpace(endpoint.ID) == "" {
				continue
			}
			candidate := resolvedCandidate{provider: provider, endpoint: endpoint}
			runtimeState, exists := r.endpointStatus(candidate, now)
			status := EndpointCircuitStatus{
				ProviderID:          provider.ID,
				ProviderName:        provider.Name,
				EndpointID:          endpoint.ID,
				EndpointURL:         endpoint.BaseURL,
				Enabled:             provider.CircuitBreaker.IsEnabled(),
				FailureThreshold:    provider.CircuitBreaker.FailureThreshold,
				ConsecutiveFailures: runtimeState.ConsecutiveFailures,
				HalfOpenInFlight:    runtimeState.HalfOpenInFlight,
				LastStatusCode:      runtimeState.LastStatusCode,
				LastError:           runtimeState.LastError,
				State:               "closed",
			}
			if exists {
				if !runtimeState.OpenUntil.IsZero() {
					next := runtimeState.OpenUntil.UTC()
					status.OpenUntil = &next
				}
				if !runtimeState.LastCheckedAt.IsZero() {
					next := runtimeState.LastCheckedAt.UTC()
					status.LastCheckedAt = &next
				}
				switch {
				case runtimeState.OpenUntil.After(now):
					status.State = "open"
				case provider.CircuitBreaker.IsEnabled() &&
					runtimeState.ConsecutiveFailures >= max(provider.CircuitBreaker.FailureThreshold, 1):
					status.State = "half_open"
				}
			}
			items = append(items, status)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].State != items[j].State {
			return circuitStateSortOrder(items[i].State) < circuitStateSortOrder(items[j].State)
		}
		if items[i].ProviderName != items[j].ProviderName {
			return items[i].ProviderName < items[j].ProviderName
		}
		if items[i].ProviderID != items[j].ProviderID {
			return items[i].ProviderID < items[j].ProviderID
		}
		return items[i].EndpointID < items[j].EndpointID
	})
	return items
}

func (r *runtimeState) ResetCircuits(state model.RoutingState, providerID, endpointID string) int {
	providerID = strings.TrimSpace(providerID)
	endpointID = strings.TrimSpace(endpointID)
	resetCount := 0
	for _, provider := range state.Providers {
		if providerID != "" && provider.ID != providerID {
			continue
		}
		for _, endpoint := range provider.Endpoints {
			if endpointID != "" && endpoint.ID != endpointID {
				continue
			}
			candidate := resolvedCandidate{provider: provider, endpoint: endpoint}
			if store, ok := r.stickyStore.(endpointRuntimeStore); ok && store != nil {
				if err := store.ResetEndpoint(candidate); err != nil {
					slog.Warn("endpoint runtime cache reset failed", "provider_id", provider.ID, "endpoint_id", endpoint.ID, "error", err)
				}
			}
			r.mu.Lock()
			delete(r.endpoints, endpointRuntimeKey(candidate))
			r.mu.Unlock()
			resetCount++
		}
	}
	return resetCount
}

func (r *runtimeState) credentialCoolingDown(candidate resolvedCandidate, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.credentials[credentialRuntimeKey(candidate)]
	if state == nil {
		return false
	}
	return state.PermanentlyDisabled || state.DisabledUntil.After(now)
}

func (r *runtimeState) acquireEndpoint(candidate resolvedCandidate, now time.Time) (bool, error) {
	if store, ok := r.stickyStore.(endpointRuntimeStore); ok && store != nil {
		halfOpen, err := store.AcquireEndpoint(candidate, now)
		if err == nil || errors.Is(err, errEndpointCircuitOpen) {
			return halfOpen, err
		}
		slog.Warn("endpoint runtime cache acquire failed", "provider_id", candidate.provider.ID, "endpoint_id", candidate.endpoint.ID, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !candidate.provider.CircuitBreaker.IsEnabled() {
		return false, nil
	}

	endpoint := r.endpointState(endpointRuntimeKey(candidate))
	if endpoint.OpenUntil.After(now) {
		return false, errEndpointCircuitOpen
	}

	threshold := candidate.provider.CircuitBreaker.FailureThreshold
	if threshold <= 0 || endpoint.ConsecutiveFailures < threshold {
		return false, nil
	}

	limit := candidate.provider.CircuitBreaker.HalfOpenMaxRequests
	if limit <= 0 {
		limit = 1
	}
	if endpoint.HalfOpenInFlight >= limit {
		return false, errEndpointCircuitOpen
	}
	endpoint.HalfOpenInFlight++
	return true, nil
}

func (r *runtimeState) reportSuccess(candidate resolvedCandidate, latency time.Duration, halfOpen bool) {
	now := time.Now().UTC()
	var stickyWrite stickyWrite
	if store, ok := r.stickyStore.(endpointRuntimeStore); ok && store != nil {
		if err := store.ReportEndpointSuccess(candidate, now, halfOpen); err != nil {
			slog.Warn("endpoint runtime cache success update failed", "provider_id", candidate.provider.ID, "endpoint_id", candidate.endpoint.ID, "error", err)
		}
	}

	r.mu.Lock()
	endpoint := r.endpointState(endpointRuntimeKey(candidate))
	if halfOpen && endpoint.HalfOpenInFlight > 0 {
		endpoint.HalfOpenInFlight--
	}
	endpoint.LastLatencyMS = latency.Milliseconds()
	endpoint.ConsecutiveFailures = 0
	endpoint.OpenUntil = time.Time{}
	endpoint.LastStatusCode = 200
	endpoint.LastError = ""
	endpoint.LastCheckedAt = now

	credential := r.credentialState(credentialRuntimeKey(candidate))
	credential.ConsecutiveFailures = 0
	credential.DisabledUntil = time.Time{}
	credential.PermanentlyDisabled = false
	credential.LastStatusCode = 200
	credential.LastError = ""
	credential.LastCheckedAt = now

	stickyWrite = r.bindStickyLocked(candidate, candidate.sessionID, now)
	r.runtimeWrites++
	if r.runtimeWrites >= defaultRuntimeSweepInterval {
		r.sweepRuntimeStateLocked(now)
		r.runtimeWrites = 0
	}
	r.mu.Unlock()

	r.persistStickyWrite(stickyWrite)
}

func (r *runtimeState) bindStickyCandidate(candidate resolvedCandidate, sessionID string, now time.Time) {
	r.mu.Lock()
	stickyWrite := r.bindStickyLocked(candidate, sessionID, now)
	r.mu.Unlock()
	r.persistStickyWrite(stickyWrite)
}

type stickyWrite struct {
	Key     string
	Binding stickyBinding
}

func (r *runtimeState) bindStickyLocked(candidate resolvedCandidate, sessionID string, now time.Time) stickyWrite {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return stickyWrite{}
	}

	key := r.stickyKey(candidate.gatewayKeyID, stickyRouteKey(candidate.route.Alias, candidate.scenario), sessionID)
	binding := stickyBinding{
		ProviderID:   candidate.provider.ID,
		EndpointID:   candidate.endpoint.ID,
		CredentialID: candidate.credential.ID,
		ExpiresAt:    now.Add(time.Duration(candidate.provider.StickySessionTTLSeconds) * time.Second),
	}
	r.sticky[key] = binding
	r.stickyWrites++
	if r.stickyWrites >= defaultStickySweepInterval {
		r.sweepExpiredStickyLocked(now)
		r.stickyWrites = 0
	}
	return stickyWrite{Key: key, Binding: binding}
}

func (r *runtimeState) persistStickyWrite(write stickyWrite) {
	if r.stickyStore == nil || strings.TrimSpace(write.Key) == "" {
		return
	}
	if err := r.stickyStore.SaveStickyBinding(write.Key, write.Binding); err != nil {
		slog.Warn("gateway sticky binding cache write failed", "key", write.Key, "error", err)
	}
}

func (r *runtimeState) reportFailure(candidate resolvedCandidate, statusCode int, errMessage string, retryAfter time.Duration, halfOpen bool) {
	now := time.Now().UTC()
	if store, ok := r.stickyStore.(endpointRuntimeStore); ok && store != nil {
		if err := store.ReportEndpointFailure(candidate, now, halfOpen); err != nil {
			slog.Warn("endpoint runtime cache failure update failed", "provider_id", candidate.provider.ID, "endpoint_id", candidate.endpoint.ID, "error", err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	endpoint := r.endpointState(endpointRuntimeKey(candidate))
	if halfOpen && endpoint.HalfOpenInFlight > 0 {
		endpoint.HalfOpenInFlight--
	}
	endpoint.LastStatusCode = statusCode
	endpoint.LastError = errMessage
	endpoint.LastCheckedAt = now
	if halfOpen && candidate.provider.CircuitBreaker.IsEnabled() {
		endpoint.ConsecutiveFailures = max(candidate.provider.CircuitBreaker.FailureThreshold, 1)
		endpoint.OpenUntil = now.Add(time.Duration(candidate.provider.CircuitBreaker.CooldownSeconds) * time.Second)
	} else {
		endpoint.ConsecutiveFailures++
	}
	if candidate.provider.CircuitBreaker.IsEnabled() && endpoint.ConsecutiveFailures >= candidate.provider.CircuitBreaker.FailureThreshold {
		endpoint.OpenUntil = now.Add(time.Duration(candidate.provider.CircuitBreaker.CooldownSeconds) * time.Second)
	}

	credential := r.credentialState(credentialRuntimeKey(candidate))
	credential.ConsecutiveFailures++
	credential.LastStatusCode = statusCode
	credential.LastError = errMessage
	credential.LastCheckedAt = now
	switch statusCode {
	case httpStatusUnauthorized, httpStatusForbidden:
		credential.PermanentlyDisabled = true
		credential.DisabledUntil = time.Time{}
	case httpStatusTooManyRequests:
		credential.PermanentlyDisabled = false
		if retryAfter <= 0 {
			retryAfter = defaultRetryAfterCooldown
		}
		credential.DisabledUntil = now.Add(clampRetryAfterCooldown(retryAfter))
	default:
		if retryAfter > 0 {
			credential.DisabledUntil = now.Add(clampRetryAfterCooldown(retryAfter))
		}
	}
	r.runtimeWrites++
	if r.runtimeWrites >= defaultRuntimeSweepInterval {
		r.sweepRuntimeStateLocked(now)
		r.runtimeWrites = 0
	}
}

func (r *runtimeState) stickyKey(gatewayKeyID, alias, sessionID string) string {
	left := strings.ToLower(strings.TrimSpace(gatewayKeyID))
	middle := strings.ToLower(strings.TrimSpace(alias))
	right := strings.ToLower(strings.TrimSpace(sessionID))
	return fmt.Sprintf("%d:%s\x00%d:%s\x00%d:%s", len(left), left, len(middle), middle, len(right), right)
}

func stickyBindingStatusFromRecord(record stickyBindingRecord) (StickyBindingStatus, bool) {
	gatewayKeyID, routeAlias, scenario, sessionID, ok := parseStickyBindingKey(record.Key)
	if !ok {
		return StickyBindingStatus{}, false
	}
	return StickyBindingStatus{
		GatewayKeyID: gatewayKeyID,
		RouteAlias:   routeAlias,
		Scenario:     scenario,
		SessionID:    sessionID,
		ProviderID:   record.Binding.ProviderID,
		EndpointID:   record.Binding.EndpointID,
		CredentialID: record.Binding.CredentialID,
		ExpiresAt:    record.Binding.ExpiresAt,
	}, true
}

func parseStickyBindingKey(key string) (gatewayKeyID, routeAlias, scenario, sessionID string, ok bool) {
	values := make([]string, 0, 3)
	for len(key) > 0 {
		colon := strings.IndexByte(key, ':')
		if colon <= 0 {
			return "", "", "", "", false
		}
		size, err := strconv.Atoi(key[:colon])
		if err != nil || size < 0 {
			return "", "", "", "", false
		}
		start := colon + 1
		end := start + size
		if end > len(key) {
			return "", "", "", "", false
		}
		values = append(values, key[start:end])
		key = key[end:]
		if key == "" {
			break
		}
		if key[0] != '\x00' {
			return "", "", "", "", false
		}
		key = key[1:]
	}
	if len(values) != 3 {
		return "", "", "", "", false
	}
	routeKey := values[1]
	routeAlias = routeKey
	if separator := strings.IndexByte(routeKey, '\x00'); separator >= 0 {
		routeAlias = routeKey[:separator]
		scenario = routeKey[separator+1:]
	}
	if values[0] == "" || routeAlias == "" || values[2] == "" {
		return "", "", "", "", false
	}
	return values[0], routeAlias, scenario, values[2], true
}

func stickyBindingMatches(record stickyBindingRecord, gatewayKeyID, routeAlias, scenario, sessionID string) bool {
	status, ok := stickyBindingStatusFromRecord(record)
	if !ok {
		return false
	}
	if wanted := strings.TrimSpace(gatewayKeyID); wanted != "" && status.GatewayKeyID != wanted {
		return false
	}
	if wanted := strings.ToLower(strings.TrimSpace(routeAlias)); wanted != "" && status.RouteAlias != wanted {
		return false
	}
	if wanted := strings.ToLower(strings.TrimSpace(scenario)); wanted != "" && status.Scenario != wanted {
		return false
	}
	if wanted := strings.ToLower(strings.TrimSpace(sessionID)); wanted != "" && status.SessionID != wanted {
		return false
	}
	return true
}

func (r *runtimeState) stickyBindingFor(gatewayKeyID, alias, sessionID string, now time.Time) (stickyBinding, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return stickyBinding{}, false
	}
	key := r.stickyKey(gatewayKeyID, alias, sessionID)

	if r.stickyStore != nil {
		binding, ok, err := r.stickyStore.LoadStickyBinding(key, now)
		if err == nil {
			r.mu.Lock()
			if ok {
				r.sticky[key] = binding
			} else {
				delete(r.sticky, key)
			}
			r.mu.Unlock()
			if ok {
				return binding, true
			}
			return stickyBinding{}, false
		}
		slog.Warn("gateway sticky binding cache read failed", "key", key, "error", err)
	}

	r.mu.Lock()
	binding, ok := r.sticky[key]
	if ok {
		if !binding.ExpiresAt.After(now) {
			delete(r.sticky, key)
			r.mu.Unlock()
			r.deleteRemoteStickyBinding(key)
			return stickyBinding{}, false
		}
		r.mu.Unlock()
		return binding, true
	}
	r.mu.Unlock()

	return stickyBinding{}, false
}

func (r *runtimeState) startSession(session LiveSessionStatus, ttl time.Duration) string {
	session.RequestID = strings.TrimSpace(session.RequestID)
	session.SessionID = strings.TrimSpace(session.SessionID)
	if session.RequestID == "" || session.SessionID == "" {
		return ""
	}
	now := time.Now().UTC()
	if session.StartedAt.IsZero() {
		session.StartedAt = now
	}
	if session.LastSeenAt.IsZero() {
		session.LastSeenAt = session.StartedAt
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	session.ExpiresAt = session.LastSeenAt.Add(ttl)

	r.mu.Lock()
	r.sweepExpiredSessionsLocked(now)
	r.sessions[session.RequestID] = session
	r.mu.Unlock()

	if store, ok := r.stickyStore.(sessionRuntimeStore); ok && store != nil {
		if err := store.SaveRuntimeSession(session.RequestID, session); err != nil {
			slog.Warn("gateway live session write failed", "request_id", session.RequestID, "error", err)
		}
	}
	return session.RequestID
}

func (r *runtimeState) touchSession(requestID string, now time.Time, ttl time.Duration) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	now = now.UTC()

	r.mu.Lock()
	r.sweepExpiredSessionsLocked(now)
	session, ok := r.sessions[requestID]
	if ok {
		session.LastSeenAt = now
		session.ExpiresAt = now.Add(ttl)
		r.sessions[requestID] = session
	}
	r.mu.Unlock()

	if !ok {
		return
	}
	if store, ok := r.stickyStore.(sessionRuntimeStore); ok && store != nil {
		if err := store.SaveRuntimeSession(requestID, session); err != nil {
			slog.Warn("gateway live session touch failed", "request_id", requestID, "error", err)
		}
	}
}

func (r *runtimeState) endSession(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, requestID)
	r.mu.Unlock()

	if store, ok := r.stickyStore.(sessionRuntimeStore); ok && store != nil {
		if err := store.DeleteRuntimeSession(requestID); err != nil {
			slog.Warn("gateway live session delete failed", "request_id", requestID, "error", err)
		}
	}
}

func (r *runtimeState) sweepExpiredStickyLocked(now time.Time) {
	for key, binding := range r.sticky {
		if !binding.ExpiresAt.After(now) {
			delete(r.sticky, key)
		}
	}
}

func (r *runtimeState) sweepExpiredSessionsLocked(now time.Time) {
	for key, session := range r.sessions {
		if !session.ExpiresAt.After(now) {
			delete(r.sessions, key)
		}
	}
}

func (r *runtimeState) deleteRemoteStickyBinding(key string) {
	if r.stickyStore == nil || strings.TrimSpace(key) == "" {
		return
	}
	if err := r.stickyStore.DeleteStickyBinding(key); err != nil {
		slog.Warn("gateway sticky binding cache delete failed", "key", key, "error", err)
	}
}

func (r *runtimeState) acquireProvider(provider model.Provider, now time.Time) error {
	if store, ok := r.stickyStore.(providerRuntimeStore); ok && store != nil {
		err := store.AcquireProvider(provider, now)
		if err == nil ||
			errors.Is(err, errProviderRateLimit) ||
			errors.Is(err, errProviderConcurrencyLimit) ||
			errors.Is(err, errProviderHourlyBudget) ||
			errors.Is(err, errProviderDailyBudget) ||
			errors.Is(err, errProviderWeeklyBudget) ||
			errors.Is(err, errProviderMonthlyBudget) {
			return err
		}
		slog.Warn("provider runtime cache acquire failed", "provider_id", provider.ID, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	window := r.providerWindows[provider.ID]
	if window == nil {
		window = &gatewayKeyWindow{}
		r.providerWindows[provider.ID] = window
	}

	minuteBucket := now.UTC().Truncate(time.Minute)
	if window.MinuteBucket.IsZero() || !window.MinuteBucket.Equal(minuteBucket) {
		window.MinuteBucket = minuteBucket
		window.RequestCount = 0
	}
	window.SpendEvents = pruneGatewaySpendEvents(window.SpendEvents, now)
	hourlyCost, dailyCost, weeklyCost, monthlyCost := gatewaySpendTotals(window.SpendEvents, now)

	if provider.RateLimitRPM > 0 && window.RequestCount >= provider.RateLimitRPM {
		return errProviderRateLimit
	}
	if provider.MaxConcurrency > 0 && window.InFlight >= provider.MaxConcurrency {
		return errProviderConcurrencyLimit
	}
	if provider.HourlyBudgetUSD > 0 && hourlyCost >= provider.HourlyBudgetUSD {
		return errProviderHourlyBudget
	}
	if provider.DailyBudgetUSD > 0 && dailyCost >= provider.DailyBudgetUSD {
		return errProviderDailyBudget
	}
	if provider.WeeklyBudgetUSD > 0 && weeklyCost >= provider.WeeklyBudgetUSD {
		return errProviderWeeklyBudget
	}
	if provider.MonthlyBudgetUSD > 0 && monthlyCost >= provider.MonthlyBudgetUSD {
		return errProviderMonthlyBudget
	}

	window.RequestCount++
	window.InFlight++
	return nil
}

func (r *runtimeState) releaseProvider(providerID string, costUSD float64) {
	now := time.Now().UTC()
	if store, ok := r.stickyStore.(providerRuntimeStore); ok && store != nil {
		if err := store.ReleaseProvider(providerID, costUSD, now); err != nil {
			slog.Warn("provider runtime cache release failed", "provider_id", providerID, "error", err)
		} else {
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	window := r.providerWindows[providerID]
	if window == nil {
		return
	}
	if window.InFlight > 0 {
		window.InFlight--
	}
	if costUSD > 0 {
		window.SpendEvents = append(pruneGatewaySpendEvents(window.SpendEvents, now), gatewaySpendEvent{
			At:      now,
			CostUSD: costUSD,
		})
	}
}

func (r *runtimeState) acquireGatewayKey(key model.GatewayKey, now time.Time) error {
	if store, ok := r.stickyStore.(gatewayKeyRuntimeStore); ok && store != nil {
		err := store.AcquireGatewayKey(key, now)
		if err == nil || errors.Is(err, errRateLimit) || errors.Is(err, errConcurrencyLimit) || errors.Is(err, errHourlyBudget) || errors.Is(err, errDailyBudget) || errors.Is(err, errWeeklyBudget) || errors.Is(err, errMonthlyBudget) {
			return err
		}
		slog.Warn("gateway key runtime cache acquire failed", "gateway_key_id", key.ID, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	window := r.keyWindows[key.ID]
	if window == nil {
		window = &gatewayKeyWindow{}
		r.keyWindows[key.ID] = window
	}

	minuteBucket := now.UTC().Truncate(time.Minute)
	if window.MinuteBucket.IsZero() || !window.MinuteBucket.Equal(minuteBucket) {
		window.MinuteBucket = minuteBucket
		window.RequestCount = 0
	}
	window.SpendEvents = pruneGatewaySpendEvents(window.SpendEvents, now)
	hourlyCost, dailyCost, weeklyCost, monthlyCost := gatewaySpendTotals(window.SpendEvents, now)

	if key.RateLimitRPM > 0 && window.RequestCount >= key.RateLimitRPM {
		return errRateLimit
	}
	if key.MaxConcurrency > 0 && window.InFlight >= key.MaxConcurrency {
		return errConcurrencyLimit
	}
	if key.HourlyBudgetUSD > 0 && hourlyCost >= key.HourlyBudgetUSD {
		return errHourlyBudget
	}
	if key.DailyBudgetUSD > 0 && dailyCost >= key.DailyBudgetUSD {
		return errDailyBudget
	}
	if key.WeeklyBudgetUSD > 0 && weeklyCost >= key.WeeklyBudgetUSD {
		return errWeeklyBudget
	}
	if key.MonthlyBudgetUSD > 0 && monthlyCost >= key.MonthlyBudgetUSD {
		return errMonthlyBudget
	}

	window.RequestCount++
	window.InFlight++
	return nil
}

func (r *runtimeState) releaseGatewayKey(keyID string, costUSD float64) {
	now := time.Now().UTC()
	if store, ok := r.stickyStore.(gatewayKeyRuntimeStore); ok && store != nil {
		if err := store.ReleaseGatewayKey(keyID, costUSD, now); err != nil {
			slog.Warn("gateway key runtime cache release failed", "gateway_key_id", keyID, "error", err)
		} else {
			return
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	window := r.keyWindows[keyID]
	if window == nil {
		return
	}
	if window.InFlight > 0 {
		window.InFlight--
	}
	if costUSD > 0 {
		window.SpendEvents = append(pruneGatewaySpendEvents(window.SpendEvents, now), gatewaySpendEvent{
			At:      now,
			CostUSD: costUSD,
		})
	}
}

func pruneGatewaySpendEvents(events []gatewaySpendEvent, now time.Time) []gatewaySpendEvent {
	if len(events) == 0 {
		return nil
	}
	cutoff := now.Add(-30 * 24 * time.Hour)
	trimmed := events[:0]
	for _, event := range events {
		if !event.At.Before(cutoff) {
			trimmed = append(trimmed, event)
		}
	}
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}

func gatewaySpendTotals(events []gatewaySpendEvent, now time.Time) (hourly, daily, weekly, monthly float64) {
	for _, event := range events {
		age := now.Sub(event.At)
		if age <= time.Hour {
			hourly += event.CostUSD
		}
		if age <= 24*time.Hour {
			daily += event.CostUSD
		}
		if age <= 7*24*time.Hour {
			weekly += event.CostUSD
		}
		if age <= 30*24*time.Hour {
			monthly += event.CostUSD
		}
	}
	return hourly, daily, weekly, monthly
}

func (r *runtimeState) gatewayAuthSourceLocked(source string, now time.Time) bool {
	source = normalizeGatewayAuthSource(source)
	if source == "" {
		return false
	}
	if store, ok := r.stickyStore.(gatewayAuthRuntimeStore); ok && store != nil {
		locked, err := store.GatewayAuthSourceLocked(source, now)
		if err == nil {
			return locked
		}
		slog.Warn("gateway auth runtime cache read failed", "source", source, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	state := r.authFailures[source]
	if state == nil {
		return false
	}
	if !state.LockedUntil.After(now) {
		if !state.LockedUntil.IsZero() {
			state.FailureCount = 0
			state.WindowStartedAt = time.Time{}
			state.LockedUntil = time.Time{}
		}
		if state.LastFailedAt.IsZero() || state.LastFailedAt.Add(defaultAuthFailureWindow).Before(now) {
			delete(r.authFailures, source)
		}
		return false
	}
	return true
}

func (r *runtimeState) invalidGatewayLookupCached(lookupHash, candidateFingerprint string, now time.Time) bool {
	lookupHash = strings.TrimSpace(lookupHash)
	if lookupHash == "" {
		return false
	}
	if store, ok := r.stickyStore.(gatewayAuthRuntimeStore); ok && store != nil {
		cached, err := store.InvalidGatewayLookupCached(lookupHash, candidateFingerprint, now)
		if err == nil {
			return cached
		}
		slog.Warn("gateway invalid lookup cache read failed", "lookup_hash", lookupHash, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.invalidGatewayLookups[lookupHash]
	if !ok {
		return false
	}
	if !entry.ExpiresAt.After(now) {
		delete(r.invalidGatewayLookups, lookupHash)
		return false
	}
	if entry.CandidateFingerprint != candidateFingerprint {
		delete(r.invalidGatewayLookups, lookupHash)
		return false
	}
	return true
}

func (r *runtimeState) recordGatewayAuthFailure(source, lookupHash, candidateFingerprint string, now time.Time) {
	source = normalizeGatewayAuthSource(source)
	lookupHash = strings.TrimSpace(lookupHash)
	if store, ok := r.stickyStore.(gatewayAuthRuntimeStore); ok && store != nil {
		if err := store.RecordGatewayAuthFailure(source, lookupHash, candidateFingerprint, now); err == nil {
			return
		} else {
			slog.Warn("gateway auth runtime cache write failed", "source", source, "lookup_hash", lookupHash, "error", err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if source != "" {
		state := r.authFailures[source]
		if state == nil {
			state = &gatewayAuthFailureState{}
			r.authFailures[source] = state
		}
		if state.WindowStartedAt.IsZero() || now.Sub(state.WindowStartedAt) > defaultAuthFailureWindow {
			state.WindowStartedAt = now
			state.FailureCount = 0
		}
		state.FailureCount++
		state.LastFailedAt = now
		if state.LockedUntil.After(now) || state.FailureCount >= defaultAuthFailureLimit {
			state.LockedUntil = now.Add(defaultAuthLockDuration)
		}
	}

	if lookupHash != "" {
		r.invalidGatewayLookups[lookupHash] = invalidGatewayLookupState{
			CandidateFingerprint: candidateFingerprint,
			ExpiresAt:            now.Add(defaultInvalidLookupTTL),
		}
	}

	r.authWrites++
	if r.authWrites >= defaultAuthSweepInterval ||
		len(r.authFailures) > maxAuthRuntimeEntries ||
		len(r.invalidGatewayLookups) > maxAuthRuntimeEntries {
		r.sweepGatewayAuthStateLocked(now)
		r.authWrites = 0
	}
}

func (r *runtimeState) clearGatewayAuthFailures(source, lookupHash string) {
	source = normalizeGatewayAuthSource(source)
	lookupHash = strings.TrimSpace(lookupHash)
	if store, ok := r.stickyStore.(gatewayAuthRuntimeStore); ok && store != nil {
		if err := store.ClearGatewayAuthFailures(source, lookupHash); err == nil {
			return
		} else {
			slog.Warn("gateway auth runtime cache clear failed", "source", source, "lookup_hash", lookupHash, "error", err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if source != "" {
		delete(r.authFailures, source)
	}
	if lookupHash != "" {
		delete(r.invalidGatewayLookups, lookupHash)
	}
}

func (r *runtimeState) sweepGatewayAuthStateLocked(now time.Time) {
	for key, entry := range r.invalidGatewayLookups {
		if !entry.ExpiresAt.After(now) {
			delete(r.invalidGatewayLookups, key)
		}
	}
	for source, state := range r.authFailures {
		if state == nil {
			delete(r.authFailures, source)
			continue
		}
		if state.LockedUntil.After(now) {
			continue
		}
		if state.LastFailedAt.IsZero() || state.LastFailedAt.Add(defaultAuthFailureWindow).Before(now) {
			delete(r.authFailures, source)
		}
	}
}

func (r *runtimeState) nextRoundRobinOffset(key string, size int) int {
	if size <= 1 {
		return 0
	}
	if store, ok := r.stickyStore.(roundRobinCounterStore); ok && store != nil {
		next, err := store.NextRoundRobinValue(key)
		if err == nil {
			return int((next - 1) % uint64(size))
		}
		slog.Warn("gateway round robin cache read failed", "key", key, "error", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	offset := int(r.roundRobin[key] % uint64(size))
	r.roundRobin[key]++
	return offset
}

func (r *runtimeState) sweepRuntimeStateLocked(now time.Time) {
	for key, state := range r.endpoints {
		if state == nil {
			delete(r.endpoints, key)
			continue
		}
		if state.HalfOpenInFlight > 0 {
			continue
		}
		if state.OpenUntil.After(now) {
			continue
		}
		if !state.LastCheckedAt.IsZero() && state.LastCheckedAt.Add(maxEndpointRuntimeAge).After(now) {
			continue
		}
		delete(r.endpoints, key)
	}
	for key, state := range r.credentials {
		if state == nil {
			delete(r.credentials, key)
			continue
		}
		if state.PermanentlyDisabled || state.DisabledUntil.After(now) {
			continue
		}
		if !state.LastCheckedAt.IsZero() && state.LastCheckedAt.Add(maxCredentialRuntimeAge).After(now) {
			continue
		}
		delete(r.credentials, key)
	}
}

func (r *runtimeState) endpointOpenLocked(candidate resolvedCandidate, now time.Time) bool {
	if !candidate.provider.CircuitBreaker.IsEnabled() {
		return false
	}
	endpoint := r.endpoints[endpointRuntimeKey(candidate)]
	if endpoint == nil {
		return false
	}
	if endpoint.OpenUntil.After(now) {
		return true
	}
	threshold := candidate.provider.CircuitBreaker.FailureThreshold
	if threshold <= 0 || endpoint.ConsecutiveFailures < threshold {
		return false
	}
	limit := candidate.provider.CircuitBreaker.HalfOpenMaxRequests
	if limit <= 0 {
		limit = 1
	}
	return endpoint.HalfOpenInFlight >= limit
}

func circuitStateSortOrder(state string) int {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "open":
		return 0
	case "half_open":
		return 1
	default:
		return 2
	}
}

func endpointRuntimeKey(candidate resolvedCandidate) string {
	return strings.ToLower(strings.TrimSpace(candidate.provider.ID)) + "\x00" + strings.ToLower(strings.TrimSpace(candidate.endpoint.ID))
}

func credentialRuntimeKey(candidate resolvedCandidate) string {
	return strings.ToLower(strings.TrimSpace(candidate.provider.ID)) + "\x00" + strings.ToLower(strings.TrimSpace(candidate.credential.ID))
}

func clampRetryAfterCooldown(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	if delay > maxRetryAfterCooldown {
		return maxRetryAfterCooldown
	}
	return delay
}

const (
	httpStatusUnauthorized    = 401
	httpStatusForbidden       = 403
	httpStatusTooManyRequests = 429
)
