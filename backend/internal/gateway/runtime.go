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

type runtimeState struct {
	mu           sync.Mutex
	endpoints    map[string]*endpointRuntimeState
	credentials  map[string]*credentialRuntimeState
	sticky       map[string]stickyBinding
	stickyWrites int
	keyWindows   map[string]*gatewayKeyWindow
}

func newRuntimeState() *runtimeState {
	return &runtimeState{
		endpoints:   map[string]*endpointRuntimeState{},
		credentials: map[string]*credentialRuntimeState{},
		sticky:      map[string]stickyBinding{},
		keyWindows:  map[string]*gatewayKeyWindow{},
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

func endpointRuntimeKey(candidate resolvedCandidate) string {
	return strings.ToLower(strings.TrimSpace(candidate.provider.ID)) + "\x00" + strings.ToLower(strings.TrimSpace(candidate.endpoint.ID))
}

func credentialRuntimeKey(candidate resolvedCandidate) string {
	return strings.ToLower(strings.TrimSpace(candidate.provider.ID)) + "\x00" + strings.ToLower(strings.TrimSpace(candidate.credential.ID)) + "\x00" + model.LegacyGatewaySecretLookupHash(candidate.credential.APIKey)
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
