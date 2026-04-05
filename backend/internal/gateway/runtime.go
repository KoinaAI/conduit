package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const (
	defaultStickySweepInterval = 64
	defaultRetryAfterCooldown  = 2 * time.Minute
	maxRetryAfterCooldown      = 15 * time.Minute
	defaultAuthFailureWindow   = 5 * time.Minute
	defaultAuthLockDuration    = 10 * time.Minute
	defaultAuthFailureLimit    = 20
	defaultInvalidLookupTTL    = 60 * time.Second
	defaultAuthSweepInterval   = 64
	maxAuthRuntimeEntries      = 4096
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
	stickyStore           stickyBindingStore
	stickyWrites          int
	keyWindows            map[string]*gatewayKeyWindow
	roundRobin            map[string]uint64
	invalidGatewayLookups map[string]invalidGatewayLookupState
	authFailures          map[string]*gatewayAuthFailureState
	authWrites            int
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
		stickyStore:           currentStickyStore,
		keyWindows:            map[string]*gatewayKeyWindow{},
		roundRobin:            map[string]uint64{},
		invalidGatewayLookups: map[string]invalidGatewayLookupState{},
		authFailures:          map[string]*gatewayAuthFailureState{},
	}
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
	return r.endpointState(endpointRuntimeKey(candidate)).LastLatencyMS
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

func (r *runtimeState) credentialCoolingDown(candidate resolvedCandidate, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.credentialState(credentialRuntimeKey(candidate))
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

	stickyWrite = r.bindStickyLocked(candidate, candidate.sessionID, now)
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
}

func (r *runtimeState) stickyKey(gatewayKeyID, alias, sessionID string) string {
	left := strings.ToLower(strings.TrimSpace(gatewayKeyID))
	middle := strings.ToLower(strings.TrimSpace(alias))
	right := strings.ToLower(strings.TrimSpace(sessionID))
	return fmt.Sprintf("%d:%s\x00%d:%s\x00%d:%s", len(left), left, len(middle), middle, len(right), right)
}

func (r *runtimeState) stickyBindingFor(gatewayKeyID, alias, sessionID string, now time.Time) (stickyBinding, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return stickyBinding{}, false
	}
	key := r.stickyKey(gatewayKeyID, alias, sessionID)

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

	if r.stickyStore == nil {
		return stickyBinding{}, false
	}
	binding, ok, err := r.stickyStore.LoadStickyBinding(key, now)
	if err != nil {
		slog.Warn("gateway sticky binding cache read failed", "key", key, "error", err)
		return stickyBinding{}, false
	}
	if !ok {
		return stickyBinding{}, false
	}
	r.mu.Lock()
	r.sticky[key] = binding
	r.mu.Unlock()
	return binding, true
}

func (r *runtimeState) sweepExpiredStickyLocked(now time.Time) {
	for key, binding := range r.sticky {
		if !binding.ExpiresAt.After(now) {
			delete(r.sticky, key)
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

func (r *runtimeState) endpointOpenLocked(candidate resolvedCandidate, now time.Time) bool {
	if !candidate.provider.CircuitBreaker.IsEnabled() {
		return false
	}
	endpoint := r.endpointState(endpointRuntimeKey(candidate))
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
