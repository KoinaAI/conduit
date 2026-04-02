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
	scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

func (s *Service) authenticateGatewayRequest(state model.RoutingState, headers http.Header, protocol model.Protocol, alias string) (model.GatewayKey, error) {
	secret := extractGatewaySecret(headers)
	if secret == "" {
		return model.GatewayKey{}, errUnauthorized
	}
	now := time.Now().UTC()
	lookupHash := model.GatewaySecretLookupHash(secret, s.cfg.SecretLookupPepper())
	legacyLookupHash := model.LegacyGatewaySecretLookupHash(secret)
	legacyCandidates := make([]model.GatewayKey, 0, len(state.GatewayKeys))
	fastCandidates := make([]model.GatewayKey, 0, 1)
	legacyLookupCandidates := make([]model.GatewayKey, 0, 1)

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
			legacyCandidates = append(legacyCandidates, key)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(key.SecretLookupHash), []byte(lookupHash)) == 1 {
			fastCandidates = append(fastCandidates, key)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(key.SecretLookupHash), []byte(legacyLookupHash)) == 1 {
			legacyLookupCandidates = append(legacyLookupCandidates, key)
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
	for _, key := range legacyLookupCandidates {
		if !model.VerifyGatewaySecret(key.SecretHash, secret) {
			continue
		}
		if err := s.runtime.acquireGatewayKey(key, now); err != nil {
			return model.GatewayKey{}, err
		}
		return key, nil
	}
	for _, key := range legacyCandidates {
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
