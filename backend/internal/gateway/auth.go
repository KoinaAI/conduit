package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

var (
	errUnauthorized        = errors.New("unauthorized")
	errAuthLocked          = errors.New("too many failed authentication attempts")
	errEndpointCircuitOpen = errors.New("provider endpoint circuit breaker is open")
	errRateLimit           = errors.New("gateway key rpm limit exceeded")
	errConcurrencyLimit    = errors.New("gateway key concurrency limit exceeded")
	errHourlyBudget        = errors.New("gateway key hourly budget exceeded")
	errDailyBudget         = errors.New("gateway key daily budget exceeded")
	errWeeklyBudget        = errors.New("gateway key weekly budget exceeded")
	errMonthlyBudget       = errors.New("gateway key monthly budget exceeded")
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

func gatewayRequestSource(r *http.Request) string {
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (s *Service) authenticateGatewayRequest(state model.RoutingState, headers http.Header, protocol model.Protocol, alias, source string) (model.GatewayKey, error) {
	secret := extractGatewaySecret(headers)
	if secret == "" {
		slog.Warn("gateway authentication failed",
			"reason", "missing_secret",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
		)
		return model.GatewayKey{}, errUnauthorized
	}
	now := time.Now().UTC()
	source = normalizeGatewayAuthSource(source)
	if s.runtime.gatewayAuthSourceLocked(source, now) {
		slog.Warn("gateway authentication blocked",
			"reason", "source_locked",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
		)
		return model.GatewayKey{}, errAuthLocked
	}
	lookupHash := model.GatewaySecretLookupHash(secret, s.cfg.SecretLookupPepper())
	fastCandidates := make([]model.GatewayKey, 0, 1)

	for _, key := range state.GatewayKeys {
		if !key.Enabled || key.IsExpired(now) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(key.SecretLookupHash), []byte(lookupHash)) == 1 {
			fastCandidates = append(fastCandidates, key)
		}
	}

	candidateFingerprint := gatewayAuthCandidateFingerprint(fastCandidates)
	if s.runtime.invalidGatewayLookupCached(lookupHash, candidateFingerprint, now) {
		s.runtime.recordGatewayAuthFailure(source, lookupHash, candidateFingerprint, now)
		slog.Warn("gateway authentication failed",
			"reason", "cached_invalid_secret",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
		)
		return model.GatewayKey{}, errUnauthorized
	}

	key, ok := findGatewayKeyMatch(secret, fastCandidates)
	if !ok {
		s.runtime.recordGatewayAuthFailure(source, lookupHash, candidateFingerprint, now)
		slog.Warn("gateway authentication failed",
			"reason", "secret_mismatch",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
		)
		return model.GatewayKey{}, errUnauthorized
	}

	s.runtime.clearGatewayAuthFailures(source, lookupHash)
	if protocol != "" && !key.AllowsProtocol(protocol) {
		slog.Warn("gateway authentication failed",
			"reason", "protocol_denied",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
			"gateway_key_id", key.ID,
		)
		return model.GatewayKey{}, errUnauthorized
	}
	if alias != "" && !key.AllowsModel(alias) {
		slog.Warn("gateway authentication failed",
			"reason", "model_denied",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
			"gateway_key_id", key.ID,
		)
		return model.GatewayKey{}, errUnauthorized
	}
	if err := s.runtime.acquireGatewayKey(key, now); err != nil {
		slog.Warn("gateway authentication failed",
			"reason", "key_limits",
			"source", source,
			"protocol", protocol,
			"route_alias", alias,
			"gateway_key_id", key.ID,
			"error", err,
		)
		return model.GatewayKey{}, err
	}
	return key, nil
}

func findGatewayKeyMatch(secret string, groups ...[]model.GatewayKey) (model.GatewayKey, bool) {
	for _, group := range groups {
		for _, key := range group {
			if model.VerifyGatewaySecret(key.SecretHash, secret) {
				return key, true
			}
		}
	}
	return model.GatewayKey{}, false
}

func gatewayAuthCandidateFingerprint(groups ...[]model.GatewayKey) string {
	items := make([]string, 0, 8)
	for _, group := range groups {
		for _, key := range group {
			items = append(items, strings.Join([]string{
				key.ID,
				key.SecretLookupHash,
				key.SecretHash,
				key.UpdatedAt.UTC().Format(time.RFC3339Nano),
			}, "\x00"))
		}
	}
	if len(items) == 0 {
		return ""
	}
	slices.Sort(items)
	return strings.Join(items, "\n")
}

func normalizeGatewayAuthSource(source string) string {
	trimmed := strings.TrimSpace(strings.ToLower(source))
	if trimmed == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		return strings.TrimSpace(host)
	}
	return trimmed
}
