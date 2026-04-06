package gateway

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestRedisStickyBindingsAreSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	first := newRuntimeState(store)
	second := newRuntimeState(store)
	now := time.Now().UTC()
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID:                      "provider-1",
			StickySessionTTLSeconds: 300,
		},
		route: model.ModelRoute{
			Alias: "gpt-5.4",
		},
		endpoint: model.ProviderEndpoint{
			ID: "endpoint-1",
		},
		credential: model.ProviderCredential{
			ID: "credential-1",
		},
		gatewayKeyID: "gateway-key-1",
	}

	first.bindStickyCandidate(candidate, "session-1", now)

	binding, ok := second.stickyBindingFor("gateway-key-1", stickyRouteKey("gpt-5.4", ""), "session-1", now)
	if !ok {
		t.Fatal("expected sticky binding to be loaded from redis-backed cache")
	}
	if binding.ProviderID != "provider-1" || binding.EndpointID != "endpoint-1" || binding.CredentialID != "credential-1" {
		t.Fatalf("unexpected sticky binding: %+v", binding)
	}
}

func TestRedisLiveSessionsAreSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	first := newRuntimeState(store)
	second := newRuntimeState(store)
	now := time.Now().UTC()

	requestID := first.startSession(LiveSessionStatus{
		RequestID:    "req-1",
		SessionID:    "session-1",
		GatewayKeyID: "gk-1",
		ProviderID:   "provider-1",
		ProviderName: "Provider One",
		RouteAlias:   "gpt-5.4",
		Protocol:     model.ProtocolOpenAIChat,
		Transport:    "http",
		StartedAt:    now,
		LastSeenAt:   now,
	}, 2*time.Minute)

	items := second.ActiveSessions(now, 15*time.Minute, 10)
	if len(items) != 1 || items[0].RequestID != requestID || items[0].SessionID != "session-1" {
		t.Fatalf("expected shared live session, got %+v", items)
	}

	first.endSession(requestID)
	if items = second.ActiveSessions(now, 15*time.Minute, 10); len(items) != 0 {
		t.Fatalf("expected shared live session to be cleared, got %+v", items)
	}
}

func TestRedisStickyBindingsCanBeListedAndResetAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	first := newRuntimeState(store)
	second := newRuntimeState(store)
	now := time.Now().UTC()
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID:                      "provider-1",
			StickySessionTTLSeconds: 300,
		},
		route: model.ModelRoute{
			Alias: "gpt-5.4",
		},
		scenario: "codex",
		endpoint: model.ProviderEndpoint{
			ID: "endpoint-1",
		},
		credential: model.ProviderCredential{
			ID: "credential-1",
		},
		gatewayKeyID: "gateway-key-1",
	}

	first.bindStickyCandidate(candidate, "session-1", now)

	items := second.StickyBindings(now)
	if len(items) != 1 {
		t.Fatalf("expected one shared sticky binding, got %+v", items)
	}
	if items[0].Scenario != "codex" || items[0].SessionID != "session-1" {
		t.Fatalf("unexpected shared sticky binding payload: %+v", items[0])
	}
	if reset := second.ResetStickyBindings("gateway-key-1", "gpt-5.4", "codex", "session-1", now); reset != 1 {
		t.Fatalf("expected one sticky binding reset, got %d", reset)
	}
	if items = first.StickyBindings(now); len(items) != 0 {
		t.Fatalf("expected sticky binding reset to replicate across runtimes, got %+v", items)
	}
}

func TestRedisRoundRobinOffsetsAreSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)

	if got := runtimeA.nextRoundRobinOffset("route\x00band", 3); got != 0 {
		t.Fatalf("expected first shared round-robin offset 0, got %d", got)
	}
	if got := runtimeB.nextRoundRobinOffset("route\x00band", 3); got != 1 {
		t.Fatalf("expected second shared round-robin offset 1, got %d", got)
	}
	if got := runtimeA.nextRoundRobinOffset("route\x00band", 3); got != 2 {
		t.Fatalf("expected third shared round-robin offset 2, got %d", got)
	}
}

func TestRedisGatewayKeyWindowsAreSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	now := time.Now().UTC()
	key := model.GatewayKey{
		ID:              "gk-1",
		RateLimitRPM:    2,
		MaxConcurrency:  1,
		HourlyBudgetUSD: 1,
	}

	if err := runtimeA.acquireGatewayKey(key, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := runtimeB.acquireGatewayKey(key, now); err != errConcurrencyLimit {
		t.Fatalf("expected shared concurrency limit, got %v", err)
	}
	runtimeA.releaseGatewayKey(key.ID, 1)

	if err := runtimeB.acquireGatewayKey(key, now); err != errHourlyBudget {
		t.Fatalf("expected shared hourly budget limit, got %v", err)
	}
}

func TestRedisProviderWindowsAreSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	now := time.Now().UTC()
	provider := model.Provider{
		ID:              "provider-1",
		RateLimitRPM:    2,
		MaxConcurrency:  1,
		HourlyBudgetUSD: 1,
	}

	if err := runtimeA.acquireProvider(provider, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := runtimeB.acquireProvider(provider, now); err != errProviderConcurrencyLimit {
		t.Fatalf("expected shared provider concurrency limit, got %v", err)
	}
	runtimeA.releaseProvider(provider.ID, 1)

	if err := runtimeB.acquireProvider(provider, now); err != errProviderHourlyBudget {
		t.Fatalf("expected shared provider hourly budget limit, got %v", err)
	}
}

func TestRedisProviderUsageListingReflectsSharedRuntimeWindows(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	now := time.Now().UTC()
	provider := model.Provider{
		ID:           "provider-1",
		Name:         "Provider One",
		RateLimitRPM: 5,
	}
	state := model.RoutingState{Providers: []model.Provider{provider}}

	if err := runtimeA.acquireProvider(provider, now); err != nil {
		t.Fatalf("acquire provider: %v", err)
	}
	items := runtimeB.ProviderUsage(state, now, 10)
	if len(items) != 1 || items[0].InFlight != 1 || items[0].CurrentMinuteRequests != 1 {
		t.Fatalf("expected shared provider usage during inflight request, got %+v", items)
	}

	runtimeA.releaseProvider(provider.ID, 1.5)
	items = runtimeB.ProviderUsage(state, now, 10)
	if len(items) != 1 || items[0].InFlight != 0 || items[0].HourlyCostUSD != 1.5 {
		t.Fatalf("expected shared provider usage after release, got %+v", items)
	}
}

func TestRedisProviderUsageTracksRequestsWithoutRPMLimit(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	now := time.Now().UTC()
	provider := model.Provider{
		ID:   "provider-1",
		Name: "Provider One",
	}
	state := model.RoutingState{Providers: []model.Provider{provider}}

	if err := runtimeA.acquireProvider(provider, now); err != nil {
		t.Fatalf("acquire provider without rpm limit: %v", err)
	}

	items := runtimeB.ProviderUsage(state, now, 10)
	if len(items) != 1 || items[0].CurrentMinuteRequests != 1 {
		t.Fatalf("expected provider usage to track requests without rpm limit, got %+v", items)
	}
}

func TestRedisInflightHeartbeatKeepsDistributedConcurrencyAlive(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	now := time.Now().UTC()
	gatewayKey := model.GatewayKey{
		ID:             "gk-1",
		MaxConcurrency: 1,
	}
	provider := model.Provider{
		ID:             "provider-1",
		MaxConcurrency: 1,
	}

	if err := runtimeA.acquireGatewayKey(gatewayKey, now); err != nil {
		t.Fatalf("acquire gateway key: %v", err)
	}
	if err := runtimeA.acquireProvider(provider, now); err != nil {
		t.Fatalf("acquire provider: %v", err)
	}

	redisServer.FastForward(59 * time.Minute)
	runtimeA.touchInFlightLeases(gatewayKey.ID, provider.ID, time.Hour)
	redisServer.FastForward(59 * time.Minute)

	if err := runtimeB.acquireGatewayKey(gatewayKey, now); err != errConcurrencyLimit {
		t.Fatalf("expected gateway key concurrency to remain enforced after heartbeat, got %v", err)
	}
	if err := runtimeB.acquireProvider(provider, now); err != errProviderConcurrencyLimit {
		t.Fatalf("expected provider concurrency to remain enforced after heartbeat, got %v", err)
	}
}

func TestRedisGatewayAuthStateIsSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	now := time.Now().UTC()
	source := "203.0.113.10"
	lookupHash := "lookup-hash"
	fingerprint := "candidate-fingerprint"

	for attempt := 0; attempt < defaultAuthFailureLimit; attempt++ {
		runtimeA.recordGatewayAuthFailure(source, lookupHash, fingerprint, now)
	}
	if !runtimeB.gatewayAuthSourceLocked(source, now) {
		t.Fatal("expected shared auth lockout state")
	}
	if !runtimeB.invalidGatewayLookupCached(lookupHash, fingerprint, now) {
		t.Fatal("expected shared invalid lookup cache")
	}

	runtimeB.clearGatewayAuthFailures(source, lookupHash)
	if runtimeA.gatewayAuthSourceLocked(source, now) {
		t.Fatal("expected shared auth lockout to clear")
	}
	if runtimeA.invalidGatewayLookupCached(lookupHash, fingerprint, now) {
		t.Fatal("expected shared invalid lookup cache to clear")
	}
}

func TestRedisCircuitBreakerHalfOpenStateIsSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	enabled := true
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID: "provider-1",
			CircuitBreaker: model.CircuitBreakerConfig{
				Enabled:             &enabled,
				FailureThreshold:    1,
				CooldownSeconds:     1,
				HalfOpenMaxRequests: 1,
			},
		},
		endpoint:   model.ProviderEndpoint{ID: "endpoint-1"},
		credential: model.ProviderCredential{ID: "credential-1", APIKey: "provider-key"},
	}

	runtimeA.reportFailure(candidate, 500, "boom", 0, false)
	if !runtimeB.endpointOpen(candidate, time.Now().UTC()) {
		t.Fatal("expected shared circuit breaker to open")
	}

	probeAt := time.Now().UTC().Add(2 * time.Second)
	halfOpen, err := runtimeA.acquireEndpoint(candidate, probeAt)
	if err != nil {
		t.Fatalf("acquire shared half-open probe: %v", err)
	}
	if !halfOpen {
		t.Fatal("expected shared probe acquisition to enter half-open mode")
	}
	if !runtimeB.endpointOpen(candidate, probeAt) {
		t.Fatal("expected shared half-open slot exhaustion to block peers")
	}
	if _, err := runtimeB.acquireEndpoint(candidate, probeAt); err != errEndpointCircuitOpen {
		t.Fatalf("expected peer half-open acquire to fail, got %v", err)
	}

	runtimeA.reportSuccess(candidate, 5*time.Millisecond, halfOpen)
	if runtimeB.endpointOpen(candidate, probeAt) {
		t.Fatal("expected successful shared half-open probe to close the circuit")
	}
}

func TestRedisCircuitStatusPreservesDiagnosticsAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	enabled := true
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID: "provider-1",
			CircuitBreaker: model.CircuitBreakerConfig{
				Enabled:          &enabled,
				FailureThreshold: 1,
				CooldownSeconds:  1,
			},
		},
		endpoint:   model.ProviderEndpoint{ID: "endpoint-1"},
		credential: model.ProviderCredential{ID: "credential-1", APIKey: "provider-key"},
	}

	runtimeA.reportFailure(candidate, 502, "boom", 0, false)

	failed, ok := runtimeB.endpointStatus(candidate, time.Now().UTC())
	if !ok {
		t.Fatal("expected shared circuit status after failure")
	}
	if failed.LastStatusCode != 502 || failed.LastError != "boom" || failed.LastCheckedAt.IsZero() {
		t.Fatalf("expected failure diagnostics to replicate, got %+v", failed)
	}

	runtimeA.reportSuccess(candidate, 5*time.Millisecond, false)

	recovered, ok := runtimeB.endpointStatus(candidate, time.Now().UTC())
	if !ok {
		t.Fatal("expected closed circuit status to retain last health metadata")
	}
	if recovered.ConsecutiveFailures != 0 || recovered.LastStatusCode != 200 || recovered.LastError != "" || recovered.LastCheckedAt.IsZero() {
		t.Fatalf("expected success diagnostics to replicate, got %+v", recovered)
	}
}

func TestRedisCircuitStatusPersistsDiagnosticsWhenCircuitBreakerDisabled(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	runtimeA := newRuntimeState(store)
	runtimeB := newRuntimeState(store)
	candidate := resolvedCandidate{
		provider:   model.Provider{ID: "provider-1"},
		endpoint:   model.ProviderEndpoint{ID: "endpoint-1"},
		credential: model.ProviderCredential{ID: "credential-1", APIKey: "provider-key"},
	}

	runtimeA.reportFailure(candidate, 503, "transient", 0, false)

	failed, ok := runtimeB.endpointStatus(candidate, time.Now().UTC())
	if !ok {
		t.Fatal("expected runtime diagnostics to persist even when circuit breaker is disabled")
	}
	if failed.LastStatusCode != 503 || failed.LastError != "transient" || failed.LastCheckedAt.IsZero() {
		t.Fatalf("expected disabled-circuit diagnostics to replicate, got %+v", failed)
	}

	runtimeA.reportSuccess(candidate, 3*time.Millisecond, false)

	recovered, ok := runtimeB.endpointStatus(candidate, time.Now().UTC())
	if !ok {
		t.Fatal("expected success diagnostics to remain queryable when circuit breaker is disabled")
	}
	if recovered.ConsecutiveFailures != 0 || recovered.LastStatusCode != 200 || recovered.LastError != "" || recovered.LastCheckedAt.IsZero() {
		t.Fatalf("expected disabled-circuit success diagnostics to replicate, got %+v", recovered)
	}
}
