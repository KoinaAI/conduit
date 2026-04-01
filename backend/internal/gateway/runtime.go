package gateway

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/example/universal-ai-gateway/internal/model"
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
	CooldownUntil       time.Time
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
	mu          sync.Mutex
	endpoints   map[string]*endpointRuntimeState
	credentials map[string]*credentialRuntimeState
	sticky      map[string]stickyBinding
	keyWindows  map[string]*gatewayKeyWindow
}

func newRuntimeState() *runtimeState {
	return &runtimeState{
		endpoints:   map[string]*endpointRuntimeState{},
		credentials: map[string]*credentialRuntimeState{},
		sticky:      map[string]stickyBinding{},
		keyWindows:  map[string]*gatewayKeyWindow{},
	}
}

func (r *runtimeState) endpointState(id string) *endpointRuntimeState {
	state := r.endpoints[id]
	if state == nil {
		state = &endpointRuntimeState{}
		r.endpoints[id] = state
	}
	return state
}

func (r *runtimeState) credentialState(id string) *credentialRuntimeState {
	state := r.credentials[id]
	if state == nil {
		state = &credentialRuntimeState{}
		r.credentials[id] = state
	}
	return state
}

func (r *runtimeState) endpointLatency(id string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endpointState(id).LastLatencyMS
}

func (r *runtimeState) endpointOpen(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.endpointState(id).OpenUntil.After(now)
}

func (r *runtimeState) credentialCoolingDown(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.credentialState(id).CooldownUntil.After(now)
}

func (r *runtimeState) reportSuccess(candidate resolvedCandidate, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	endpoint := r.endpointState(candidate.endpoint.ID)
	endpoint.LastLatencyMS = latency.Milliseconds()
	endpoint.ConsecutiveFailures = 0
	endpoint.OpenUntil = time.Time{}
	endpoint.LastStatusCode = 200
	endpoint.LastError = ""
	endpoint.LastCheckedAt = time.Now().UTC()

	credential := r.credentialState(candidate.credential.ID)
	credential.ConsecutiveFailures = 0
	credential.CooldownUntil = time.Time{}
	credential.LastStatusCode = 200
	credential.LastError = ""

	if candidate.sessionID != "" {
		r.sticky[r.stickyKey(candidate.gatewayKeyID, candidate.route.Alias, candidate.sessionID)] = stickyBinding{
			ProviderID:   candidate.provider.ID,
			EndpointID:   candidate.endpoint.ID,
			CredentialID: candidate.credential.ID,
			ExpiresAt:    time.Now().UTC().Add(time.Duration(candidate.provider.StickySessionTTLSeconds) * time.Second),
		}
	}
}

func (r *runtimeState) reportFailure(candidate resolvedCandidate, statusCode int, errMessage string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	endpoint := r.endpointState(candidate.endpoint.ID)
	endpoint.ConsecutiveFailures++
	endpoint.LastStatusCode = statusCode
	endpoint.LastError = errMessage
	endpoint.LastCheckedAt = now
	if candidate.provider.CircuitBreaker.IsEnabled() && endpoint.ConsecutiveFailures >= candidate.provider.CircuitBreaker.FailureThreshold {
		endpoint.OpenUntil = now.Add(time.Duration(candidate.provider.CircuitBreaker.CooldownSeconds) * time.Second)
	}

	credential := r.credentialState(candidate.credential.ID)
	credential.ConsecutiveFailures++
	credential.LastStatusCode = statusCode
	credential.LastError = errMessage
	if statusCode == 429 || statusCode == 401 || statusCode == 403 {
		credential.CooldownUntil = now.Add(2 * time.Minute)
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
