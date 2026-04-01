package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/gateway"
	"github.com/KoinaAI/conduit/backend/internal/integration"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

// Handlers serves the administrative API surface.
type Handlers struct {
	cfg         config.Config
	store       *store.FileStore
	integration *integration.Service
	gateway     *gateway.Service
}

// New creates the admin handler set.
func New(cfg config.Config, store *store.FileStore, integration *integration.Service, gatewayService ...*gateway.Service) *Handlers {
	var currentGateway *gateway.Service
	if len(gatewayService) > 0 {
		currentGateway = gatewayService[0]
	}
	return &Handlers{
		cfg:         cfg,
		store:       store,
		integration: integration,
		gateway:     currentGateway,
	}
}

// Middleware enforces the admin bearer token for /api/admin/* routes.
func (h *Handlers) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		token := r.Header.Get("X-Admin-Token")
		if token == "" {
			token = bearerToken(r.Header.Get("Authorization"))
		}
		if token != h.cfg.AdminToken {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetState serves the compatibility snapshot used by the current frontend.
func (h *Handlers) GetState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot())
}

// PutState replaces the compatibility snapshot while preserving advanced fields
// that the current frontend does not yet understand.
func (h *Handlers) PutState(w http.ResponseWriter, r *http.Request) {
	var next model.State
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		mergeCompatibilityState(state, &next)
		*state = next
		state.Normalize()
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// GetMeta serves lightweight control-plane metadata.
func (h *Handlers) GetMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enable_realtime": h.cfg.EnableRealtime,
		"server_time":     time.Now().UTC(),
		"openapi_url":     "/api/admin/openapi.json",
	})
}

// OpenAPI serves a hand-maintained OpenAPI document for the RESTful admin API.
func (h *Handlers) OpenAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildOpenAPISpec())
}

// SyncIntegration executes a remote sync for one integration.
func (h *Handlers) SyncIntegration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snapshot := h.store.Snapshot()
	current, ok := snapshot.FindIntegration(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "integration not found"})
		return
	}
	result := h.integration.PrepareSync(r.Context(), current)
	var applyErr error
	saved, err := h.updateState(func(state *model.State) error {
		_, applyErr = h.integration.ApplySyncResult(state, id, result)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if applyErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": applyErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// CheckinIntegration executes the daily check-in flow for one integration.
func (h *Handlers) CheckinIntegration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snapshot := h.store.Snapshot()
	current, ok := snapshot.FindIntegration(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "integration not found"})
		return
	}
	result := h.integration.PrepareCheckin(r.Context(), current)
	var applyErr error
	saved, err := h.updateState(func(state *model.State) error {
		applyErr = h.integration.ApplyCheckinResult(state, id, result)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if applyErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": applyErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// SyncAllIntegrations executes sync for all enabled integrations.
func (h *Handlers) SyncAllIntegrations(w http.ResponseWriter, r *http.Request) {
	snapshot := h.store.Snapshot()
	type syncOperation struct {
		id     string
		result integration.SyncResult
	}

	operations := make([]syncOperation, 0, len(snapshot.Integrations))
	var firstErr error
	for _, current := range snapshot.Integrations {
		if !current.Enabled {
			continue
		}
		result := h.integration.PrepareSync(r.Context(), current)
		if firstErr == nil && result.Err != nil {
			firstErr = result.Err
		}
		operations = append(operations, syncOperation{id: current.ID, result: result})
	}

	saved, err := h.updateState(func(state *model.State) error {
		for _, operation := range operations {
			if _, err := h.integration.ApplySyncResult(state, operation.id, operation.result); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if firstErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": firstErr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// ListProviders serves GET /api/admin/providers.
func (h *Handlers) ListProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().Providers)
}

// CreateProvider serves POST /api/admin/providers.
func (h *Handlers) CreateProvider(w http.ResponseWriter, r *http.Request) {
	var payload model.Provider
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if payload.ID == "" {
		payload.ID = model.NewID("provider")
	}
	payload.CreatedAt = time.Now().UTC()
	payload.UpdatedAt = payload.CreatedAt
	saved, err := h.updateState(func(state *model.State) error {
		state.Providers = append([]model.Provider{payload}, state.Providers...)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	provider, _ := saved.FindProvider(payload.ID)
	writeJSON(w, http.StatusCreated, provider)
}

// GetProvider serves GET /api/admin/providers/{id}.
func (h *Handlers) GetProvider(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.store.Snapshot().FindProvider(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}
	writeJSON(w, http.StatusOK, provider)
}

// UpdateProvider serves PUT /api/admin/providers/{id}.
func (h *Handlers) UpdateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload model.Provider
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	payload.ID = id
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.Providers {
			if state.Providers[index].ID != id {
				continue
			}
			payload.CreatedAt = state.Providers[index].CreatedAt
			payload.UpdatedAt = time.Now().UTC()
			state.Providers[index] = payload
			return nil
		}
		return errNotFound("provider")
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	provider, _ := saved.FindProvider(id)
	writeJSON(w, http.StatusOK, provider)
}

// DeleteProvider serves DELETE /api/admin/providers/{id}.
func (h *Handlers) DeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.updateState(func(state *model.State) error {
		before := len(state.Providers)
		state.Providers = slicesDeleteProvider(state.Providers, id)
		if len(state.Providers) == before {
			return errNotFound("provider")
		}
		for routeIndex := range state.ModelRoutes {
			route := &state.ModelRoutes[routeIndex]
			filtered := route.Targets[:0]
			for _, target := range route.Targets {
				if target.AccountID != id {
					filtered = append(filtered, target)
				}
			}
			route.Targets = filtered
		}
		return nil
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListRoutes serves GET /api/admin/routes.
func (h *Handlers) ListRoutes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().ModelRoutes)
}

// CreateRoute serves POST /api/admin/routes.
func (h *Handlers) CreateRoute(w http.ResponseWriter, r *http.Request) {
	var payload model.ModelRoute
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		state.ModelRoutes = append([]model.ModelRoute{payload}, state.ModelRoutes...)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	route, _ := saved.FindRoute(payload.Alias)
	writeJSON(w, http.StatusCreated, route)
}

// GetRoute serves GET /api/admin/routes/{alias}.
func (h *Handlers) GetRoute(w http.ResponseWriter, r *http.Request) {
	route, ok := h.store.Snapshot().FindRoute(r.PathValue("alias"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "route not found"})
		return
	}
	writeJSON(w, http.StatusOK, route)
}

// UpdateRoute serves PUT /api/admin/routes/{alias}.
func (h *Handlers) UpdateRoute(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	var payload model.ModelRoute
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.ModelRoutes {
			if !strings.EqualFold(state.ModelRoutes[index].Alias, alias) {
				continue
			}
			state.ModelRoutes[index] = payload
			return nil
		}
		return errNotFound("route")
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	route, _ := saved.FindRoute(payload.Alias)
	writeJSON(w, http.StatusOK, route)
}

// DeleteRoute serves DELETE /api/admin/routes/{alias}.
func (h *Handlers) DeleteRoute(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	_, err := h.updateState(func(state *model.State) error {
		before := len(state.ModelRoutes)
		filtered := state.ModelRoutes[:0]
		for _, route := range state.ModelRoutes {
			if !strings.EqualFold(route.Alias, alias) {
				filtered = append(filtered, route)
			}
		}
		state.ModelRoutes = filtered
		if len(state.ModelRoutes) == before {
			return errNotFound("route")
		}
		return nil
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListPricingProfiles serves GET /api/admin/pricing-profiles.
func (h *Handlers) ListPricingProfiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().PricingProfiles)
}

// CreatePricingProfile serves POST /api/admin/pricing-profiles.
func (h *Handlers) CreatePricingProfile(w http.ResponseWriter, r *http.Request) {
	var payload model.PricingProfile
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		state.PricingProfiles = append([]model.PricingProfile{payload}, state.PricingProfiles...)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	profile, _ := saved.FindPricingProfile(payload.ID)
	writeJSON(w, http.StatusCreated, profile)
}

// UpdatePricingProfile serves PUT /api/admin/pricing-profiles/{id}.
func (h *Handlers) UpdatePricingProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload model.PricingProfile
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	payload.ID = id
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.PricingProfiles {
			if state.PricingProfiles[index].ID == id {
				state.PricingProfiles[index] = payload
				return nil
			}
		}
		return errNotFound("pricing profile")
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	profile, _ := saved.FindPricingProfile(id)
	writeJSON(w, http.StatusOK, profile)
}

// DeletePricingProfile serves DELETE /api/admin/pricing-profiles/{id}.
func (h *Handlers) DeletePricingProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.updateState(func(state *model.State) error {
		before := len(state.PricingProfiles)
		filtered := state.PricingProfiles[:0]
		for _, profile := range state.PricingProfiles {
			if profile.ID != id {
				filtered = append(filtered, profile)
			}
		}
		state.PricingProfiles = filtered
		if len(state.PricingProfiles) == before {
			return errNotFound("pricing profile")
		}
		return nil
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListIntegrations serves GET /api/admin/integrations.
func (h *Handlers) ListIntegrations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().Integrations)
}

// CreateIntegration serves POST /api/admin/integrations.
func (h *Handlers) CreateIntegration(w http.ResponseWriter, r *http.Request) {
	var payload model.Integration
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if payload.ID == "" {
		payload.ID = model.NewID("integration")
	}
	payload.UpdatedAt = time.Now().UTC()
	saved, err := h.updateState(func(state *model.State) error {
		state.Integrations = append([]model.Integration{payload}, state.Integrations...)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	integration, _ := saved.FindIntegration(payload.ID)
	writeJSON(w, http.StatusCreated, integration)
}

// UpdateIntegration serves PUT /api/admin/integrations/{id}.
func (h *Handlers) UpdateIntegration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload model.Integration
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	payload.ID = id
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.Integrations {
			if state.Integrations[index].ID == id {
				payload.UpdatedAt = time.Now().UTC()
				state.Integrations[index] = payload
				return nil
			}
		}
		return errNotFound("integration")
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	integration, _ := saved.FindIntegration(id)
	writeJSON(w, http.StatusOK, integration)
}

// DeleteIntegration serves DELETE /api/admin/integrations/{id}.
func (h *Handlers) DeleteIntegration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.updateState(func(state *model.State) error {
		before := len(state.Integrations)
		filtered := state.Integrations[:0]
		for _, integration := range state.Integrations {
			if integration.ID != id {
				filtered = append(filtered, integration)
			}
		}
		state.Integrations = filtered
		if len(state.Integrations) == before {
			return errNotFound("integration")
		}
		return nil
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListGatewayKeys serves GET /api/admin/gateway-keys.
func (h *Handlers) ListGatewayKeys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().GatewayKeys)
}

// CreateGatewayKey serves POST /api/admin/gateway-keys and returns the newly
// generated plaintext secret once.
func (h *Handlers) CreateGatewayKey(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name             string           `json:"name"`
		Secret           string           `json:"secret"`
		Enabled          *bool            `json:"enabled"`
		ExpiresAt        *time.Time       `json:"expires_at"`
		AllowedModels    []string         `json:"allowed_models"`
		AllowedProtocols []model.Protocol `json:"allowed_protocols"`
		MaxConcurrency   int              `json:"max_concurrency"`
		RateLimitRPM     int              `json:"rate_limit_rpm"`
		DailyBudgetUSD   float64          `json:"daily_budget_usd"`
		Notes            string           `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	secret := strings.TrimSpace(payload.Secret)
	if secret == "" {
		var err error
		secret, err = model.NewGatewaySecret()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	hash, err := model.HashGatewaySecret(secret)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	now := time.Now().UTC()
	created := model.GatewayKey{
		ID:               model.NewID("gk"),
		Name:             strings.TrimSpace(payload.Name),
		SecretHash:       hash,
		SecretPreview:    model.SecretPreview(secret),
		Enabled:          enabled,
		ExpiresAt:        payload.ExpiresAt,
		AllowedModels:    payload.AllowedModels,
		AllowedProtocols: payload.AllowedProtocols,
		MaxConcurrency:   payload.MaxConcurrency,
		RateLimitRPM:     payload.RateLimitRPM,
		DailyBudgetUSD:   payload.DailyBudgetUSD,
		Notes:            payload.Notes,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if payload.Name == "" {
		created.Name = created.ID
	}
	saved, err := h.updateState(func(state *model.State) error {
		state.GatewayKeys = append([]model.GatewayKey{created}, state.GatewayKeys...)
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	key, _ := saved.FindGatewayKey(created.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"gateway_key": key,
		"secret":      secret,
	})
}

// DeleteGatewayKey serves DELETE /api/admin/gateway-keys/{id}.
func (h *Handlers) DeleteGatewayKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.updateState(func(state *model.State) error {
		before := len(state.GatewayKeys)
		filtered := state.GatewayKeys[:0]
		for _, key := range state.GatewayKeys {
			if key.ID != id {
				filtered = append(filtered, key)
			}
		}
		state.GatewayKeys = filtered
		if len(state.GatewayKeys) == before {
			return errNotFound("gateway key")
		}
		return nil
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListRequestHistory serves GET /api/admin/request-history.
func (h *Handlers) ListRequestHistory(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().RequestHistory)
}

// GetRequestAttempts serves GET /api/admin/request-history/{id}/attempts.
func (h *Handlers) GetRequestAttempts(w http.ResponseWriter, r *http.Request) {
	attempts, err := h.store.RequestAttempts(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, attempts)
}

// ProbeAllProviders executes the active endpoint probe flow.
func (h *Handlers) ProbeAllProviders(w http.ResponseWriter, r *http.Request) {
	if h.gateway == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "gateway probe service not configured"})
		return
	}
	result := h.RunProbes(r.Context())
	writeJSON(w, http.StatusOK, result)
}

// RunCheckins executes background daily check-ins.
func (h *Handlers) RunCheckins(ctx context.Context) {
	now := time.Now().UTC()
	snapshot := h.store.Snapshot()
	results := h.integration.PrepareDailyCheckins(ctx, snapshot, now)
	if len(results) == 0 {
		return
	}

	_, _ = h.store.Update(func(state *model.State) error {
		_ = h.integration.ApplyDailyCheckins(state, results)
		return nil
	})
}

// RunProbes executes active endpoint probes and returns the probe report.
func (h *Handlers) RunProbes(ctx context.Context) map[string]any {
	if h.gateway == nil {
		return map[string]any{
			"checked_at": time.Now().UTC(),
			"results":    []any{},
			"error":      "gateway probe service not configured",
		}
	}
	return h.gateway.RunProbes(ctx)
}

func (h *Handlers) updateState(mutate func(*model.State) error) (model.State, error) {
	saved, err := h.store.Update(mutate)
	if err != nil {
		return model.State{}, err
	}
	return saved, nil
}

func mergeCompatibilityState(current, next *model.State) {
	next.Normalize()
	current.Normalize()

	if len(next.GatewayKeys) == 0 {
		next.GatewayKeys = current.GatewayKeys
	}
	for nextProviderIndex := range next.Providers {
		nextProvider := &next.Providers[nextProviderIndex]
		currentProvider, ok := current.FindProvider(nextProvider.ID)
		if !ok {
			continue
		}
		if len(nextProvider.Endpoints) == 0 {
			nextProvider.Endpoints = currentProvider.Endpoints
		}
		if len(nextProvider.Credentials) == 0 {
			nextProvider.Credentials = currentProvider.Credentials
		}
		if nextProvider.RoutingMode == "" {
			nextProvider.RoutingMode = currentProvider.RoutingMode
		}
		if nextProvider.MaxAttempts <= 0 {
			nextProvider.MaxAttempts = currentProvider.MaxAttempts
		}
		if nextProvider.StickySessionTTLSeconds <= 0 {
			nextProvider.StickySessionTTLSeconds = currentProvider.StickySessionTTLSeconds
		}
		if nextProvider.CircuitBreaker == (model.CircuitBreakerConfig{}) {
			nextProvider.CircuitBreaker = currentProvider.CircuitBreaker
		}
		nextProvider.CreatedAt = currentProvider.CreatedAt
	}
}

func buildOpenAPISpec() map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Conduit Admin API",
			"version":     "2026-04-01",
			"description": "RESTful administrative surface for providers, routes, pricing, integrations, gateway keys, request history, and maintenance operations.",
		},
		"paths": map[string]any{
			"/api/admin/providers": map[string]any{
				"get":  map[string]any{"summary": "List providers"},
				"post": map[string]any{"summary": "Create provider"},
			},
			"/api/admin/routes": map[string]any{
				"get":  map[string]any{"summary": "List routes"},
				"post": map[string]any{"summary": "Create route"},
			},
			"/api/admin/pricing-profiles": map[string]any{
				"get":  map[string]any{"summary": "List pricing profiles"},
				"post": map[string]any{"summary": "Create pricing profile"},
			},
			"/api/admin/integrations": map[string]any{
				"get":  map[string]any{"summary": "List integrations"},
				"post": map[string]any{"summary": "Create integration"},
			},
			"/api/admin/gateway-keys": map[string]any{
				"get":  map[string]any{"summary": "List gateway keys"},
				"post": map[string]any{"summary": "Create gateway key"},
			},
			"/api/admin/request-history": map[string]any{
				"get": map[string]any{"summary": "List request history"},
			},
			"/api/admin/maintenance/probes": map[string]any{
				"post": map[string]any{"summary": "Probe all provider endpoints"},
			},
			"/api/admin/openapi.json": map[string]any{
				"get": map[string]any{"summary": "OpenAPI document"},
			},
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func bearerToken(value string) string {
	const prefix = "Bearer "
	if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):]
	}
	return value
}

type resourceError struct {
	name string
}

func (e resourceError) Error() string {
	return e.name + " not found"
}

func errNotFound(name string) error {
	return resourceError{name: name}
}

func writeResourceError(w http.ResponseWriter, err error) {
	var resource resourceError
	if ok := errors.As(err, &resource); ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

func slicesDeleteProvider(providers []model.Provider, id string) []model.Provider {
	filtered := providers[:0]
	for _, provider := range providers {
		if provider.ID != id {
			filtered = append(filtered, provider)
		}
	}
	return filtered
}
