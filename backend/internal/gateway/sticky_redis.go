package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
)

const redisStickyOperationTimeout = 200 * time.Millisecond
const redisEndpointRuntimeTTL = 24 * time.Hour

type redisStickyStore struct {
	client *redis.Client
	prefix string
}

type redisEndpointCircuitState struct {
	Failures         int
	OpenUntilUnixMS  int64
	HalfOpenInFlight int
	LastStatusCode   int
	LastError        string
	LastCheckedAtMS  int64
}

var redisDecrementFloorZeroScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
	return 0
end
local value = tonumber(current)
if not value or value <= 1 then
	redis.call("DEL", KEYS[1])
	return 0
end
return redis.call("DECR", KEYS[1])
`)

func newRedisStickyStore(cfg config.Config) stickyBindingStore {
	if strings.TrimSpace(cfg.RedisAddr) == "" {
		return nil
	}
	return &redisStickyStore{
		client: redis.NewClient(&redis.Options{
			Addr:     strings.TrimSpace(cfg.RedisAddr),
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		}),
		prefix: strings.TrimSpace(cfg.RedisKeyPrefix),
	}
}

func (s *redisStickyStore) LoadStickyBinding(key string, now time.Time) (stickyBinding, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	payload, err := s.client.Get(ctx, s.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return stickyBinding{}, false, nil
	}
	if err != nil {
		return stickyBinding{}, false, err
	}

	var binding stickyBinding
	if err := json.Unmarshal(payload, &binding); err != nil {
		_ = s.DeleteStickyBinding(key)
		return stickyBinding{}, false, err
	}
	if !binding.ExpiresAt.After(now) {
		_ = s.DeleteStickyBinding(key)
		return stickyBinding{}, false, nil
	}
	return binding, true, nil
}

func (s *redisStickyStore) SaveStickyBinding(key string, binding stickyBinding) error {
	ttl := time.Until(binding.ExpiresAt)
	if ttl <= 0 {
		return nil
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Set(ctx, s.key(key), payload, ttl).Err()
}

func (s *redisStickyStore) DeleteStickyBinding(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Del(ctx, s.key(key)).Err()
}

func (s *redisStickyStore) SaveRuntimeSession(key string, session LiveSessionStatus) error {
	ttl := time.Until(session.ExpiresAt)
	if ttl <= 0 {
		return nil
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Set(ctx, s.runtimeSessionKey(key), payload, ttl).Err()
}

func (s *redisStickyStore) DeleteRuntimeSession(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Del(ctx, s.runtimeSessionKey(key)).Err()
}

func (s *redisStickyStore) ListRuntimeSessions(now time.Time) ([]runtimeSessionRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	pattern := s.runtimeSessionPrefix() + "*"
	cursor := uint64(0)
	records := make([]runtimeSessionRecord, 0, 16)
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, redisKey := range keys {
			payload, err := s.client.Get(ctx, redisKey).Bytes()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return nil, err
			}

			var session LiveSessionStatus
			if err := json.Unmarshal(payload, &session); err != nil {
				_ = s.client.Del(ctx, redisKey).Err()
				continue
			}
			if !session.ExpiresAt.After(now) {
				_ = s.client.Del(ctx, redisKey).Err()
				continue
			}

			records = append(records, runtimeSessionRecord{
				Key:     strings.TrimPrefix(redisKey, s.runtimeSessionPrefix()),
				Session: session,
			})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return records, nil
}

func (s *redisStickyStore) ListStickyBindings(now time.Time) ([]stickyBindingRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	pattern := s.stickyPrefix() + "*"
	cursor := uint64(0)
	records := make([]stickyBindingRecord, 0, 16)
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, redisKey := range keys {
			payload, err := s.client.Get(ctx, redisKey).Bytes()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return nil, err
			}

			var binding stickyBinding
			if err := json.Unmarshal(payload, &binding); err != nil {
				_ = s.client.Del(ctx, redisKey).Err()
				continue
			}
			if !binding.ExpiresAt.After(now) {
				_ = s.client.Del(ctx, redisKey).Err()
				continue
			}

			records = append(records, stickyBindingRecord{
				Key:     strings.TrimPrefix(redisKey, s.stickyPrefix()),
				Binding: binding,
			})
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return records, nil
}

func (s *redisStickyStore) NextRoundRobinValue(key string) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Incr(ctx, s.roundRobinKey(key)).Uint64()
}

func (s *redisStickyStore) EndpointOpen(candidate resolvedCandidate, now time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	state, err := s.loadEndpointCircuitState(ctx, s.endpointCircuitKey(candidate))
	if err != nil {
		return false, err
	}
	return redisEndpointOpen(candidate, state, now), nil
}

func (s *redisStickyStore) AcquireEndpoint(candidate resolvedCandidate, now time.Time) (bool, error) {
	if !candidate.provider.CircuitBreaker.IsEnabled() {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	key := s.endpointCircuitKey(candidate)
	for attempt := 0; attempt < 3; attempt++ {
		resultHalfOpen := false
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			state, err := s.loadEndpointCircuitState(ctx, key)
			if err != nil {
				return err
			}
			if state.OpenUntilUnixMS > now.UTC().UnixMilli() {
				return errEndpointCircuitOpen
			}
			threshold := candidate.provider.CircuitBreaker.FailureThreshold
			if threshold <= 0 || state.Failures < threshold {
				resultHalfOpen = false
				return nil
			}
			limit := candidate.provider.CircuitBreaker.HalfOpenMaxRequests
			if limit <= 0 {
				limit = 1
			}
			if state.HalfOpenInFlight >= limit {
				return errEndpointCircuitOpen
			}
			state.HalfOpenInFlight++
			resultHalfOpen = true
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				fields := map[string]any{
					"failures":           state.Failures,
					"open_until_unixms":  state.OpenUntilUnixMS,
					"half_open_inflight": state.HalfOpenInFlight,
					"last_status_code":   state.LastStatusCode,
					"last_error":         state.LastError,
					"last_checked_atms":  state.LastCheckedAtMS,
				}
				pipe.HSet(ctx, key, fields)
				pipe.Expire(ctx, key, redisEndpointRuntimeTTL)
				return nil
			})
			return err
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return false, err
		}
		return resultHalfOpen, nil
	}
	return false, redis.TxFailedErr
}

func (s *redisStickyStore) ReportEndpointSuccess(candidate resolvedCandidate, now time.Time, halfOpen bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	key := s.endpointCircuitKey(candidate)
	for attempt := 0; attempt < 3; attempt++ {
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			state, err := s.loadEndpointCircuitState(ctx, key)
			if err != nil {
				return err
			}
			if halfOpen && state.HalfOpenInFlight > 0 {
				state.HalfOpenInFlight--
			}
			state.Failures = 0
			state.OpenUntilUnixMS = 0
			state.HalfOpenInFlight = 0
			state.LastStatusCode = 200
			state.LastError = ""
			state.LastCheckedAtMS = now.UTC().UnixMilli()
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				fields := map[string]any{
					"failures":           state.Failures,
					"open_until_unixms":  state.OpenUntilUnixMS,
					"half_open_inflight": state.HalfOpenInFlight,
					"last_status_code":   state.LastStatusCode,
					"last_error":         state.LastError,
					"last_checked_atms":  state.LastCheckedAtMS,
				}
				pipe.HSet(ctx, key, fields)
				pipe.Expire(ctx, key, redisEndpointRuntimeTTL)
				return nil
			})
			return err
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return err
	}
	return redis.TxFailedErr
}

func (s *redisStickyStore) ReportEndpointFailure(candidate resolvedCandidate, now time.Time, statusCode int, errMessage string, halfOpen bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	key := s.endpointCircuitKey(candidate)
	for attempt := 0; attempt < 3; attempt++ {
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			state, err := s.loadEndpointCircuitState(ctx, key)
			if err != nil {
				return err
			}
			if halfOpen && candidate.provider.CircuitBreaker.IsEnabled() {
				state.Failures = max(candidate.provider.CircuitBreaker.FailureThreshold, 1)
				state.HalfOpenInFlight = 0
				state.OpenUntilUnixMS = now.Add(time.Duration(candidate.provider.CircuitBreaker.CooldownSeconds) * time.Second).UTC().UnixMilli()
			} else {
				state.Failures++
				state.HalfOpenInFlight = 0
				if candidate.provider.CircuitBreaker.IsEnabled() && state.Failures >= candidate.provider.CircuitBreaker.FailureThreshold {
					state.OpenUntilUnixMS = now.Add(time.Duration(candidate.provider.CircuitBreaker.CooldownSeconds) * time.Second).UTC().UnixMilli()
				}
			}
			state.LastStatusCode = statusCode
			state.LastError = strings.TrimSpace(errMessage)
			state.LastCheckedAtMS = now.UTC().UnixMilli()
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				fields := map[string]any{
					"failures":           state.Failures,
					"open_until_unixms":  state.OpenUntilUnixMS,
					"half_open_inflight": state.HalfOpenInFlight,
					"last_status_code":   state.LastStatusCode,
					"last_error":         state.LastError,
					"last_checked_atms":  state.LastCheckedAtMS,
				}
				pipe.HSet(ctx, key, fields)
				pipe.Expire(ctx, key, redisEndpointRuntimeTTL)
				return nil
			})
			return err
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return err
	}
	return redis.TxFailedErr
}

func (s *redisStickyStore) LoadEndpointState(candidate resolvedCandidate) (endpointRuntimeState, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	state, err := s.loadEndpointCircuitState(ctx, s.endpointCircuitKey(candidate))
	if err != nil {
		return endpointRuntimeState{}, false, err
	}
	runtimeState := endpointRuntimeState{
		ConsecutiveFailures: state.Failures,
		HalfOpenInFlight:    state.HalfOpenInFlight,
		LastStatusCode:      state.LastStatusCode,
		LastError:           state.LastError,
	}
	if state.OpenUntilUnixMS > 0 {
		runtimeState.OpenUntil = time.UnixMilli(state.OpenUntilUnixMS).UTC()
	}
	if state.LastCheckedAtMS > 0 {
		runtimeState.LastCheckedAt = time.UnixMilli(state.LastCheckedAtMS).UTC()
	}
	exists := state.Failures > 0 ||
		state.OpenUntilUnixMS > 0 ||
		state.HalfOpenInFlight > 0 ||
		state.LastStatusCode > 0 ||
		strings.TrimSpace(state.LastError) != "" ||
		state.LastCheckedAtMS > 0
	if !exists {
		return endpointRuntimeState{}, false, nil
	}
	return runtimeState, true, nil
}

func (s *redisStickyStore) ResetEndpoint(candidate resolvedCandidate) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Del(ctx, s.endpointCircuitKey(candidate)).Err()
}

func (s *redisStickyStore) AcquireGatewayKey(key model.GatewayKey, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	if err := s.enforceGatewayBudgets(ctx, key, now); err != nil {
		return err
	}

	rateKey := ""
	if key.RateLimitRPM > 0 {
		rateKey = s.gatewayRateKey(key.ID, now)
		count, err := s.client.Incr(ctx, rateKey).Result()
		if err != nil {
			return err
		}
		_ = s.client.Expire(ctx, rateKey, 2*time.Minute).Err()
		if count > int64(key.RateLimitRPM) {
			_, _ = s.client.Decr(ctx, rateKey).Result()
			return errRateLimit
		}
	}

	if key.MaxConcurrency > 0 {
		inflightKey := s.gatewayInFlightKey(key.ID)
		count, err := s.client.Incr(ctx, inflightKey).Result()
		if err != nil {
			if rateKey != "" {
				_, _ = s.client.Decr(ctx, rateKey).Result()
			}
			return err
		}
		_ = s.client.Expire(ctx, inflightKey, time.Hour).Err()
		if count > int64(key.MaxConcurrency) {
			_, _ = s.client.Decr(ctx, inflightKey).Result()
			if rateKey != "" {
				_, _ = s.client.Decr(ctx, rateKey).Result()
			}
			return errConcurrencyLimit
		}
	}

	return nil
}

func (s *redisStickyStore) ReleaseGatewayKey(keyID string, costUSD float64, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	if strings.TrimSpace(keyID) == "" {
		return nil
	}
	if err := s.decrementCounterFloorZero(ctx, s.gatewayInFlightKey(keyID)); err != nil {
		return err
	}
	if costUSD <= 0 {
		return nil
	}
	member := s.gatewaySpendMember(now, costUSD)
	if err := s.client.ZAdd(ctx, s.gatewaySpendKey(keyID), redis.Z{
		Score:  float64(now.UTC().UnixMilli()),
		Member: member,
	}).Err(); err != nil {
		return err
	}
	_ = s.client.Expire(ctx, s.gatewaySpendKey(keyID), 31*24*time.Hour).Err()
	return nil
}

func (s *redisStickyStore) TouchGatewayKeyInFlight(keyID string, ttl time.Duration) error {
	if strings.TrimSpace(keyID) == "" || ttl <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Expire(ctx, s.gatewayInFlightKey(keyID), ttl).Err()
}

func (s *redisStickyStore) AcquireProvider(provider model.Provider, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	if err := s.enforceProviderBudgets(ctx, provider, now); err != nil {
		return err
	}

	rateKey := s.providerRateKey(provider.ID, now)
	count, err := s.client.Incr(ctx, rateKey).Result()
	if err != nil {
		return err
	}
	_ = s.client.Expire(ctx, rateKey, 2*time.Minute).Err()
	if provider.RateLimitRPM > 0 && count > int64(provider.RateLimitRPM) {
		_, _ = s.client.Decr(ctx, rateKey).Result()
		return errProviderRateLimit
	}

	inflightKey := s.providerInFlightKey(provider.ID)
	count, err = s.client.Incr(ctx, inflightKey).Result()
	if err != nil {
		_, _ = s.client.Decr(ctx, rateKey).Result()
		return err
	}
	_ = s.client.Expire(ctx, inflightKey, time.Hour).Err()
	if provider.MaxConcurrency > 0 && count > int64(provider.MaxConcurrency) {
		_, _ = s.client.Decr(ctx, inflightKey).Result()
		_, _ = s.client.Decr(ctx, rateKey).Result()
		return errProviderConcurrencyLimit
	}

	return nil
}

func (s *redisStickyStore) ReleaseProvider(providerID string, costUSD float64, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	if strings.TrimSpace(providerID) == "" {
		return nil
	}
	if err := s.decrementCounterFloorZero(ctx, s.providerInFlightKey(providerID)); err != nil {
		return err
	}
	if costUSD <= 0 {
		return nil
	}
	member := s.gatewaySpendMember(now, costUSD)
	if err := s.client.ZAdd(ctx, s.providerSpendKey(providerID), redis.Z{
		Score:  float64(now.UTC().UnixMilli()),
		Member: member,
	}).Err(); err != nil {
		return err
	}
	_ = s.client.Expire(ctx, s.providerSpendKey(providerID), 31*24*time.Hour).Err()
	return nil
}

func (s *redisStickyStore) TouchProviderInFlight(providerID string, ttl time.Duration) error {
	if strings.TrimSpace(providerID) == "" || ttl <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	return s.client.Expire(ctx, s.providerInFlightKey(providerID), ttl).Err()
}

func (s *redisStickyStore) LoadProviderRuntime(provider model.Provider, now time.Time) (ProviderRuntimeStatus, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	minuteRequests, err := s.client.Get(ctx, s.providerRateKey(provider.ID, now)).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		return ProviderRuntimeStatus{}, false, err
	}
	inFlight, err := s.client.Get(ctx, s.providerInFlightKey(provider.ID)).Int()
	if err != nil && !errors.Is(err, redis.Nil) {
		return ProviderRuntimeStatus{}, false, err
	}

	spendKey := s.providerSpendKey(provider.ID)
	cutoff := strconv.FormatInt(now.Add(-30*24*time.Hour).UTC().UnixMilli(), 10)
	if err := s.client.ZRemRangeByScore(ctx, spendKey, "-inf", redisExclusiveUpperBound(cutoff)).Err(); err != nil {
		return ProviderRuntimeStatus{}, false, err
	}
	entries, err := s.client.ZRangeByScoreWithScores(ctx, spendKey, &redis.ZRangeBy{
		Min: cutoff,
		Max: "+inf",
	}).Result()
	if err != nil {
		return ProviderRuntimeStatus{}, false, err
	}

	var hourlyCost, dailyCost, weeklyCost, monthlyCost float64
	for _, entry := range entries {
		recordedAt := time.UnixMilli(int64(entry.Score)).UTC()
		cost, ok := parseGatewaySpendMember(entry.Member)
		if !ok {
			continue
		}
		age := now.Sub(recordedAt)
		if age <= time.Hour {
			hourlyCost += cost
		}
		if age <= 24*time.Hour {
			dailyCost += cost
		}
		if age <= 7*24*time.Hour {
			weeklyCost += cost
		}
		if age <= 30*24*time.Hour {
			monthlyCost += cost
		}
	}

	status := ProviderRuntimeStatus{
		ProviderID:            provider.ID,
		ProviderName:          provider.Name,
		RateLimitRPM:          provider.RateLimitRPM,
		MaxConcurrency:        provider.MaxConcurrency,
		HourlyBudgetUSD:       provider.HourlyBudgetUSD,
		DailyBudgetUSD:        provider.DailyBudgetUSD,
		WeeklyBudgetUSD:       provider.WeeklyBudgetUSD,
		MonthlyBudgetUSD:      provider.MonthlyBudgetUSD,
		CurrentMinuteRequests: minuteRequests,
		InFlight:              inFlight,
		HourlyCostUSD:         hourlyCost,
		DailyCostUSD:          dailyCost,
		WeeklyCostUSD:         weeklyCost,
		MonthlyCostUSD:        monthlyCost,
		MinuteBucketStartedAt: now.UTC().Truncate(time.Minute),
	}
	return status, providerRuntimeActive(status), nil
}

func (s *redisStickyStore) GatewayAuthSourceLocked(source string, _ time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	count, err := s.client.Exists(ctx, s.gatewayAuthLockKey(source)).Result()
	return count > 0, err
}

func (s *redisStickyStore) InvalidGatewayLookupCached(lookupHash, candidateFingerprint string, _ time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()
	value, err := s.client.Get(ctx, s.gatewayInvalidLookupKey(lookupHash)).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == candidateFingerprint, nil
}

func (s *redisStickyStore) RecordGatewayAuthFailure(source, lookupHash, candidateFingerprint string, _ time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	if source != "" {
		failKey := s.gatewayAuthFailuresKey(source)
		count, err := s.client.Incr(ctx, failKey).Result()
		if err != nil {
			return err
		}
		_ = s.client.Expire(ctx, failKey, defaultAuthFailureWindow).Err()
		if count >= defaultAuthFailureLimit {
			if err := s.client.Set(ctx, s.gatewayAuthLockKey(source), "1", defaultAuthLockDuration).Err(); err != nil {
				return err
			}
		}
	}
	if lookupHash != "" {
		if err := s.client.Set(ctx, s.gatewayInvalidLookupKey(lookupHash), candidateFingerprint, defaultInvalidLookupTTL).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *redisStickyStore) ClearGatewayAuthFailures(source, lookupHash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), redisStickyOperationTimeout)
	defer cancel()

	keys := make([]string, 0, 3)
	if source != "" {
		keys = append(keys, s.gatewayAuthFailuresKey(source), s.gatewayAuthLockKey(source))
	}
	if lookupHash != "" {
		keys = append(keys, s.gatewayInvalidLookupKey(lookupHash))
	}
	if len(keys) == 0 {
		return nil
	}
	return s.client.Del(ctx, keys...).Err()
}

func (s *redisStickyStore) key(key string) string {
	return s.stickyPrefix() + key
}

func (s *redisStickyStore) stickyPrefix() string {
	prefix := strings.TrimSpace(s.prefix)
	if prefix == "" {
		prefix = "conduit"
	}
	return prefix + ":sticky:"
}

func (s *redisStickyStore) runtimeSessionKey(key string) string {
	return s.runtimeSessionPrefix() + strings.TrimSpace(key)
}

func (s *redisStickyStore) runtimeSessionPrefix() string {
	prefix := strings.TrimSpace(s.prefix)
	if prefix == "" {
		prefix = "conduit"
	}
	return prefix + ":session:"
}

func (s *redisStickyStore) roundRobinKey(key string) string {
	prefix := strings.TrimSpace(s.prefix)
	if prefix == "" {
		prefix = "conduit"
	}
	return prefix + ":rr:" + key
}

func (s *redisStickyStore) gatewayRateKey(keyID string, now time.Time) string {
	return s.prefixedKey("gateway:rate", strings.TrimSpace(keyID), strconv.FormatInt(now.UTC().Truncate(time.Minute).Unix(), 10))
}

func (s *redisStickyStore) providerRateKey(providerID string, now time.Time) string {
	return s.prefixedKey("provider:rate", strings.TrimSpace(providerID), strconv.FormatInt(now.UTC().Truncate(time.Minute).Unix(), 10))
}

func (s *redisStickyStore) endpointCircuitKey(candidate resolvedCandidate) string {
	return s.prefixedKey("endpoint:circuit", endpointRuntimeKey(candidate))
}

func (s *redisStickyStore) gatewayInFlightKey(keyID string) string {
	return s.prefixedKey("gateway:inflight", strings.TrimSpace(keyID))
}

func (s *redisStickyStore) providerInFlightKey(providerID string) string {
	return s.prefixedKey("provider:inflight", strings.TrimSpace(providerID))
}

func (s *redisStickyStore) gatewaySpendKey(keyID string) string {
	return s.prefixedKey("gateway:spend", strings.TrimSpace(keyID))
}

func (s *redisStickyStore) providerSpendKey(providerID string) string {
	return s.prefixedKey("provider:spend", strings.TrimSpace(providerID))
}

func (s *redisStickyStore) gatewaySpendMember(now time.Time, costUSD float64) string {
	return fmt.Sprintf("%d|%.12f", now.UTC().UnixNano(), costUSD)
}

func (s *redisStickyStore) gatewayAuthFailuresKey(source string) string {
	return s.prefixedKey("auth:failures", strings.TrimSpace(source))
}

func (s *redisStickyStore) gatewayAuthLockKey(source string) string {
	return s.prefixedKey("auth:lock", strings.TrimSpace(source))
}

func (s *redisStickyStore) gatewayInvalidLookupKey(lookupHash string) string {
	return s.prefixedKey("auth:invalid", strings.TrimSpace(lookupHash))
}

func (s *redisStickyStore) prefixedKey(parts ...string) string {
	prefix := strings.TrimSpace(s.prefix)
	if prefix == "" {
		prefix = "conduit"
	}
	items := []string{prefix}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		items = append(items, part)
	}
	return strings.Join(items, ":")
}

func (s *redisStickyStore) enforceGatewayBudgets(ctx context.Context, key model.GatewayKey, now time.Time) error {
	if key.HourlyBudgetUSD <= 0 && key.DailyBudgetUSD <= 0 && key.WeeklyBudgetUSD <= 0 && key.MonthlyBudgetUSD <= 0 {
		return nil
	}
	spendKey := s.gatewaySpendKey(key.ID)
	cutoff := strconv.FormatInt(now.Add(-30*24*time.Hour).UTC().UnixMilli(), 10)
	if err := s.client.ZRemRangeByScore(ctx, spendKey, "-inf", redisExclusiveUpperBound(cutoff)).Err(); err != nil {
		return err
	}
	entries, err := s.client.ZRangeByScoreWithScores(ctx, spendKey, &redis.ZRangeBy{
		Min: cutoff,
		Max: "+inf",
	}).Result()
	if err != nil {
		return err
	}
	var hourlyCost, dailyCost, weeklyCost, monthlyCost float64
	for _, entry := range entries {
		recordedAt := time.UnixMilli(int64(entry.Score)).UTC()
		cost, ok := parseGatewaySpendMember(entry.Member)
		if !ok {
			continue
		}
		age := now.Sub(recordedAt)
		if age <= time.Hour {
			hourlyCost += cost
		}
		if age <= 24*time.Hour {
			dailyCost += cost
		}
		if age <= 7*24*time.Hour {
			weeklyCost += cost
		}
		if age <= 30*24*time.Hour {
			monthlyCost += cost
		}
	}
	switch {
	case key.HourlyBudgetUSD > 0 && hourlyCost >= key.HourlyBudgetUSD:
		return errHourlyBudget
	case key.DailyBudgetUSD > 0 && dailyCost >= key.DailyBudgetUSD:
		return errDailyBudget
	case key.WeeklyBudgetUSD > 0 && weeklyCost >= key.WeeklyBudgetUSD:
		return errWeeklyBudget
	case key.MonthlyBudgetUSD > 0 && monthlyCost >= key.MonthlyBudgetUSD:
		return errMonthlyBudget
	default:
		return nil
	}
}

func (s *redisStickyStore) enforceProviderBudgets(ctx context.Context, provider model.Provider, now time.Time) error {
	if provider.HourlyBudgetUSD <= 0 && provider.DailyBudgetUSD <= 0 && provider.WeeklyBudgetUSD <= 0 && provider.MonthlyBudgetUSD <= 0 {
		return nil
	}
	spendKey := s.providerSpendKey(provider.ID)
	cutoff := strconv.FormatInt(now.Add(-30*24*time.Hour).UTC().UnixMilli(), 10)
	if err := s.client.ZRemRangeByScore(ctx, spendKey, "-inf", redisExclusiveUpperBound(cutoff)).Err(); err != nil {
		return err
	}
	entries, err := s.client.ZRangeByScoreWithScores(ctx, spendKey, &redis.ZRangeBy{
		Min: cutoff,
		Max: "+inf",
	}).Result()
	if err != nil {
		return err
	}
	var hourlyCost, dailyCost, weeklyCost, monthlyCost float64
	for _, entry := range entries {
		recordedAt := time.UnixMilli(int64(entry.Score)).UTC()
		cost, ok := parseGatewaySpendMember(entry.Member)
		if !ok {
			continue
		}
		age := now.Sub(recordedAt)
		if age <= time.Hour {
			hourlyCost += cost
		}
		if age <= 24*time.Hour {
			dailyCost += cost
		}
		if age <= 7*24*time.Hour {
			weeklyCost += cost
		}
		if age <= 30*24*time.Hour {
			monthlyCost += cost
		}
	}
	switch {
	case provider.HourlyBudgetUSD > 0 && hourlyCost >= provider.HourlyBudgetUSD:
		return errProviderHourlyBudget
	case provider.DailyBudgetUSD > 0 && dailyCost >= provider.DailyBudgetUSD:
		return errProviderDailyBudget
	case provider.WeeklyBudgetUSD > 0 && weeklyCost >= provider.WeeklyBudgetUSD:
		return errProviderWeeklyBudget
	case provider.MonthlyBudgetUSD > 0 && monthlyCost >= provider.MonthlyBudgetUSD:
		return errProviderMonthlyBudget
	default:
		return nil
	}
}

func parseGatewaySpendMember(member any) (float64, bool) {
	text, ok := member.(string)
	if !ok {
		return 0, false
	}
	_, rawCost, found := strings.Cut(text, "|")
	if !found {
		return 0, false
	}
	cost, err := strconv.ParseFloat(strings.TrimSpace(rawCost), 64)
	if err != nil {
		return 0, false
	}
	return cost, true
}

func (s *redisStickyStore) loadEndpointCircuitState(ctx context.Context, key string) (redisEndpointCircuitState, error) {
	values, err := s.client.HGetAll(ctx, key).Result()
	if err != nil {
		return redisEndpointCircuitState{}, err
	}
	if len(values) == 0 {
		return redisEndpointCircuitState{}, nil
	}
	return redisEndpointCircuitState{
		Failures:         parseRedisInt(values["failures"]),
		OpenUntilUnixMS:  parseRedisInt64(values["open_until_unixms"]),
		HalfOpenInFlight: parseRedisInt(values["half_open_inflight"]),
		LastStatusCode:   parseRedisInt(values["last_status_code"]),
		LastError:        values["last_error"],
		LastCheckedAtMS:  parseRedisInt64(values["last_checked_atms"]),
	}, nil
}

func redisEndpointOpen(candidate resolvedCandidate, state redisEndpointCircuitState, now time.Time) bool {
	if !candidate.provider.CircuitBreaker.IsEnabled() {
		return false
	}
	if state.OpenUntilUnixMS > now.UTC().UnixMilli() {
		return true
	}
	threshold := candidate.provider.CircuitBreaker.FailureThreshold
	if threshold <= 0 || state.Failures < threshold {
		return false
	}
	limit := candidate.provider.CircuitBreaker.HalfOpenMaxRequests
	if limit <= 0 {
		limit = 1
	}
	return state.HalfOpenInFlight >= limit
}

func parseRedisInt(value string) int {
	number, _ := strconv.Atoi(strings.TrimSpace(value))
	return number
}

func parseRedisInt64(value string) int64 {
	number, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return number
}

func redisExclusiveUpperBound(value string) string {
	return "(" + strings.TrimSpace(value)
}

func (s *redisStickyStore) decrementCounterFloorZero(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return redisDecrementFloorZeroScript.Run(ctx, s.client, []string{key}).Err()
}

func (s *redisStickyStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}
