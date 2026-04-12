package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

var (
	errUnauthorized     = errors.New("unauthorized")
	errRateLimit        = errors.New("gateway key rpm limit exceeded")
	errConcurrencyLimit = errors.New("gateway key concurrency limit exceeded")
	errDailyBudget      = errors.New("gateway key daily budget exceeded")
)

type gatewayAuthContextKey struct{}

func withGatewayKeyContext(ctx context.Context, key model.GatewayKey) context.Context {
	return context.WithValue(ctx, gatewayAuthContextKey{}, key)
}

// GatewayKeyFromContext returns the authenticated gateway key when present.
func GatewayKeyFromContext(ctx context.Context) (model.GatewayKey, bool) {
	key, ok := ctx.Value(gatewayAuthContextKey{}).(model.GatewayKey)
	return key, ok
}

func extractGatewaySecret(headers http.Header) string {
	if value := strings.TrimSpace(headers.Get("X-API-Key")); value != "" {
		return value
	}
	if value := strings.TrimSpace(headers.Get("Authorization")); value != "" {
		return bearerToken(value)
	}
	return ""
}

func bearerToken(value string) string {
	const prefix = "Bearer "
	if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):]
	}
	return value
}

func (s *Service) authenticateGatewayRequest(state model.RoutingState, headers http.Header, protocol model.Protocol, alias string) (model.GatewayKey, error) {
	secret := extractGatewaySecret(headers)
	if secret == "" {
		return model.GatewayKey{}, errUnauthorized
	}
	now := time.Now().UTC()
	lookupHash := model.GatewaySecretLookupHash(secret, s.cfg.SecretLookupPepper())
	fastCandidates := make([]model.GatewayKey, 0, 1)

	for _, key := range state.GatewayKeys {
		if !key.Enabled || key.IsExpired(now) {
			continue
		}
		if protocol != "" && !key.AllowsProtocol(protocol) {
			continue
		}
		if alias != "" && !key.AllowsModel(alias) {
			continue
		}
		if key.SecretLookupHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(key.SecretLookupHash), []byte(lookupHash)) == 1 {
			fastCandidates = append(fastCandidates, key)
		}
	}

	for _, key := range fastCandidates {
		if !model.VerifyGatewaySecret(key.SecretHash, secret) {
			continue
		}
		if err := s.runtime.acquireGatewayKey(key, now); err != nil {
			return model.GatewayKey{}, err
		}
		return key, nil
	}
	return model.GatewayKey{}, errUnauthorized
}
