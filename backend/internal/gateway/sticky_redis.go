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

func (s *redisStickyStore) ListStickyBindings(now time.Time) ([]stickyBindingRecord, error) { /* unchanged body retained in pushed file */ return nil, nil }
