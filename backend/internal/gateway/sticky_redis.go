package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/KoinaAI/conduit/backend/internal/config"
)

const redisStickyOperationTimeout = 200 * time.Millisecond

type redisStickyStore struct {
	client *redis.Client
	prefix string
}

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

func (s *redisStickyStore) key(key string) string {
	prefix := strings.TrimSpace(s.prefix)
	if prefix == "" {
		prefix = "conduit"
	}
	return prefix + ":sticky:" + key
}

func (s *redisStickyStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}
