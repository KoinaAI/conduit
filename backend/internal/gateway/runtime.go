package gateway

import (
	"fmt"
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

type gatewayKeyWindow struct {
	MinuteBucket time.Time
	RequestCount int
	DailyBucket  time.Time
	DailyCostUSD float64
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
	stickyWrites          int
	keyWindows            map[string]*gatewayKeyWindow
	roundRobin            map[string]uint64
	invalidGatewayLookups map[string]invalidGatewayLookupState
	authFailures          map[string]*gatewayAuthFailureState
	authWrites            int
}

func newRuntimeState() *runtimeState {
	return &runtimeState{
		endpoints:             map[string]*endpointRuntimeState{},
		credentials:           map[string]*credentialRuntimeState{},
		sticky:                map[string]stickyBinding{},
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endpointState(endpointRuntimeKey(candidate)).OpenUntil.After(now)
}

func (r *runtimeState) credentialCoolingDown(candidate resolvedCandidate, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.credentialState(credentialRuntimeKey(candidate))
	return state.PermanentlyDisabled || state.DisabledUntil.After(now)
}

func (r *runtimeState) reportSuccess(candidate resolvedCandidate, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	endpoint := r.endpointState(endpointRuntimeKey(candidate))
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

	if candidate.sessionID == "" {
		return
	}

	r.sticky[r.stickyKey(candidate.gatewayKeyID, candidate.route.Alias, candidate.sessionID)] = stickyBinding{
		ProviderID:   candidate.provider.ID,
		EndpointID:   candidate.endpoint.ID,
		CredentialID: candidate.credential.ID,
		ExpiresAt:    now.Add(time.Duration(candidate.provider.StickySessionTTLSeconds) * time.Second),
	}
	r.stickyWrites++
	if r.stickyWrites >= defaultStickySweepInterval {
		r.sweepExpiredStickyLocked(now)
		r.stickyWrites = 0
	}
}

func (r *runtimeState) reportFailure(candidate resolvedCandidate, statusCode int, errMessage string, retryAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	endpoint := r.endpointState(endpointRuntimeKey(candidate))
	endpoint.ConsecutiveFailures++
	endpoint.LastStatusCode = statusCode
	endpoint.LastError = errMessage
	endpoint.LastCheckedAt = now
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
	r.mu.Lock()
	defer r.mu.Unlock()

	key := r.stickyKey(gatewayKeyID, alias, sessionID)
	binding, ok := r.sticky[key]
	if !ok {
		return stickyBinding{}, false
	}
	if !binding.ExpiresAt.After(now) {
		delete(r.sticky, key)
		return stickyBinding{}, false
	}
	return binding, true
}

func (r *runtimeState) sweepExpiredStickyLocked(now time.Time) {
	for key, binding := range r.sticky {
		if !binding.ExpiresAt.After(now) {
			delete(r.sticky, key)
		}
	}
}

func (r *runtimeState) acquireGatewayKey(key model.GatewayKey, now time.Time) error {
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
	dailyBucket := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if window.DailyBucket.IsZero() || !window.DailyBucket.Equal(dailyBucket) {
		window.DailyBucket = dailyBucket
		window.DailyCostUSD = 0
	}

	if key.RateLimitRPM > 0 && window.RequestCount >= key.RateLimitRPM {
		return errRateLimit
	}
	if key.MaxConcurrency > 0 && window.InFlight >= key.MaxConcurrency {
		return errConcurrencyLimit
	}
	if key.DailyBudgetUSD > 0 && window.DailyCostUSD >= key.DailyBudgetUSD {
		return errDailyBudget
	}

	window.RequestCount++
	window.InFlight++
	return nil
}

func (r *runtimeState) releaseGatewayKey(keyID string, costUSD float64) {
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
		window.DailyCostUSD += costUSD
	}
}

func (r *runtimeState) gatewayAuthSourceLocked(source string, now time.Time) bool {
	source = normalizeGatewayAuthSource(source)
	if source == "" {
		return false
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

	r.mu.Lock()
	defer r.mu.Unlock()

	offset := int(r.roundRobin[key] % uint64(size))
	r.roundRobin[key]++
	return offset
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
