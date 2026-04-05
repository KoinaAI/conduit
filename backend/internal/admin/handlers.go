package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/gateway"
	"github.com/KoinaAI/conduit/backend/internal/integration"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/pricing"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

// Handlers serves the administrative API surface.
type Handlers struct {
	cfg         config.Config
	store       *store.FileStore
	integration *integration.Service
	pricing     *pricing.Service
	gateway     *gateway.Service
}

const maxAdminBodyBytes = 1 << 20

// New creates the admin handler set.
func New(cfg config.Config, store *store.FileStore, integration *integration.Service, gatewayService ...*gateway.Service) *Handlers {
	var currentGateway *gateway.Service
	if len(gatewayService) > 0 {
		currentGateway = gatewayService[0]
	}
	var pricingService *pricing.Service
	if cfg.PricingSyncEnabled {
		pricingService = pricing.NewService(cfg.PricingCatalogURL, nil)
	}
	return &Handlers{
		cfg:         cfg,
		store:       store,
		integration: integration,
		pricing:     pricingService,
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
		adminToken := strings.TrimSpace(h.cfg.AdminToken)
		if adminToken == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "admin API is not configured"})
			return
		}
		token := strings.TrimSpace(r.Header.Get("X-Admin-Token"))
		if token == "" {
			token = bearerToken(r.Header.Get("Authorization"))
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminToken)) != 1 {
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
	if err := decodeJSONBody(w, r, &next); err != nil {
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
		"enable_realtime":      h.cfg.EnableRealtime,
		"pricing_sync_enabled": h.cfg.PricingSyncEnabled,
		"pricing_catalog_url":  strings.TrimSpace(h.cfg.PricingCatalogURL),
		"server_time":          time.Now().UTC(),
		"openapi_url":          "/api/admin/openapi.json",
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
	if result.Err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": result.Err.Error()})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		_, err := h.integration.ApplySyncResult(state, id, result)
		return err
	})
	if err != nil {
		writeResourceError(w, err)
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
	indexByID := map[string]int{}
	for _, current := range snapshot.Integrations {
		if !current.Enabled {
			continue
		}
		operations = append(operations, syncOperation{id: current.ID})
		indexByID[current.ID] = len(operations) - 1
	}
	if len(operations) == 0 {
		writeJSON(w, http.StatusOK, snapshot)
		return
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, current := range snapshot.Integrations {
		if !current.Enabled {
			continue
		}
		current := current
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := h.integration.PrepareSync(r.Context(), current)
			mu.Lock()
			operations[indexByID[current.ID]].result = result
			mu.Unlock()
		}()
	}
	wg.Wait()

	successful := make([]syncOperation, 0, len(operations))
	failures := make([]map[string]any, 0)
	for _, operation := range operations {
		if operation.result.Err != nil {
			failures = append(failures, map[string]any{
				"integration_id": operation.id,
				"error":          operation.result.Err.Error(),
			})
			continue
		}
		successful = append(successful, operation)
	}
	if len(successful) == 0 {
		message := "all integration sync operations failed"
		if len(failures) == 1 {
			message = failures[0]["error"].(string)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":    message,
			"failures": failures,
		})
		return
	}

	saved, err := h.updateState(func(state *model.State) error {
		for _, operation := range successful {
			if _, err := h.integration.ApplySyncResult(state, operation.id, operation.result); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeResourceError(w, err)
		return
	}
	if len(failures) == 0 {
		writeJSON(w, http.StatusOK, saved)
		return
	}
	writeJSON(w, http.StatusMultiStatus, map[string]any{
		"state":    saved,
		"failures": failures,
	})
}

// ListProviders serves GET /api/admin/providers.
func (h *Handlers) ListProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot().Providers)
}

// CreateProvider serves POST /api/admin/providers.
func (h *Handlers) CreateProvider(w http.ResponseWriter, r *http.Request) {
	var payload model.Provider
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := validateProviderProxyURL(payload.ProxyURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if payload.ID == "" {
		payload.ID = model.NewID("provider")
	}
	payload.CreatedAt = time.Now().UTC()
	payload.UpdatedAt = payload.CreatedAt
	saved, err := h.updateState(func(state *model.State) error {
		payload = preserveProviderSecrets(model.Provider{}, payload)
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := validateProviderProxyURL(payload.ProxyURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	payload.ID = id
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.Providers {
			if state.Providers[index].ID != id {
				continue
			}
			payload = preserveProviderSecrets(state.Providers[index], payload)
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
		removeProviderReferences(state, id)
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		payload.Alias = strings.TrimSpace(payload.Alias)
		if !validateRouteStrategy(&payload) {
			return errValidation("route strategy or transformers are invalid")
		}
		if err := validateRouteAlias(state.ModelRoutes, payload.Alias, ""); err != nil {
			return err
		}
		state.ModelRoutes = append([]model.ModelRoute{payload}, state.ModelRoutes...)
		return nil
	})
	if err != nil {
		writeMutationError(w, err)
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	payload.Alias = strings.TrimSpace(payload.Alias)
	if payload.Alias == "" {
		payload.Alias = strings.TrimSpace(alias)
	}
	if !validateRouteStrategy(&payload) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "route strategy or transformers are invalid"})
		return
	}
	saved, err := h.updateState(func(state *model.State) error {
		if err := validateRouteAlias(state.ModelRoutes, payload.Alias, alias); err != nil {
			return err
		}
		for index := range state.ModelRoutes {
			if !strings.EqualFold(state.ModelRoutes[index].Alias, alias) {
				continue
			}
			if len(payload.Scenarios) == 0 {
				payload.Scenarios = state.ModelRoutes[index].Scenarios
			}
			state.ModelRoutes[index] = payload
			return nil
		}
		return errNotFound("route")
	})
	if err != nil {
		writeMutationError(w, err)
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
		removeGatewayKeyModelReferences(state.GatewayKeys, map[string]struct{}{
			strings.ToLower(strings.TrimSpace(alias)): {},
		})
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := h.integration.ValidateBaseURL(payload.BaseURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if payload.ID == "" {
		payload.ID = model.NewID("integration")
	}
	payload.UpdatedAt = time.Now().UTC()
	saved, err := h.updateState(func(state *model.State) error {
		payload = preserveIntegrationSecrets(model.Integration{}, payload)
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := h.integration.ValidateBaseURL(payload.BaseURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	payload.ID = id
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.Integrations {
			if state.Integrations[index].ID == id {
				payload = preserveIntegrationSecrets(state.Integrations[index], payload)
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
		linkedProviderID := ""
		filtered := state.Integrations[:0]
		for _, integration := range state.Integrations {
			if integration.ID == id {
				linkedProviderID = strings.TrimSpace(integration.LinkedProviderID)
			}
			if integration.ID != id {
				filtered = append(filtered, integration)
			}
		}
		state.Integrations = filtered
		if len(state.Integrations) == before {
			return errNotFound("integration")
		}
		if linkedProviderID != "" {
			state.Providers = slicesDeleteProvider(state.Providers, linkedProviderID)
			removeProviderReferences(state, linkedProviderID)
		}
		removeIntegrationPricingProfiles(state, id)
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
		HourlyBudgetUSD  float64          `json:"hourly_budget_usd"`
		DailyBudgetUSD   float64          `json:"daily_budget_usd"`
		WeeklyBudgetUSD  float64          `json:"weekly_budget_usd"`
		MonthlyBudgetUSD float64          `json:"monthly_budget_usd"`
		Notes            string           `json:"notes"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
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
	} else if err := validateGatewaySecretStrength(secret); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
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
		SecretLookupHash: model.GatewaySecretLookupHash(secret, h.cfg.SecretLookupPepper()),
		SecretPreview:    model.SecretPreview(secret),
		Enabled:          enabled,
		ExpiresAt:        payload.ExpiresAt,
		AllowedModels:    payload.AllowedModels,
		AllowedProtocols: payload.AllowedProtocols,
		MaxConcurrency:   payload.MaxConcurrency,
		RateLimitRPM:     payload.RateLimitRPM,
		HourlyBudgetUSD:  payload.HourlyBudgetUSD,
		DailyBudgetUSD:   payload.DailyBudgetUSD,
		WeeklyBudgetUSD:  payload.WeeklyBudgetUSD,
		MonthlyBudgetUSD: payload.MonthlyBudgetUSD,
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

// UpdateGatewayKey serves PUT /api/admin/gateway-keys/{id}.
func (h *Handlers) UpdateGatewayKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload struct {
		Name             *string         `json:"name"`
		Secret           *string         `json:"secret"`
		Enabled          *bool           `json:"enabled"`
		ExpiresAt        json.RawMessage `json:"expires_at"`
		AllowedModels    json.RawMessage `json:"allowed_models"`
		AllowedProtocols json.RawMessage `json:"allowed_protocols"`
		MaxConcurrency   *int            `json:"max_concurrency"`
		RateLimitRPM     *int            `json:"rate_limit_rpm"`
		HourlyBudgetUSD  *float64        `json:"hourly_budget_usd"`
		DailyBudgetUSD   *float64        `json:"daily_budget_usd"`
		WeeklyBudgetUSD  *float64        `json:"weekly_budget_usd"`
		MonthlyBudgetUSD *float64        `json:"monthly_budget_usd"`
		Notes            *string         `json:"notes"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	var rotatedSecret string
	saved, err := h.updateState(func(state *model.State) error {
		for index := range state.GatewayKeys {
			if state.GatewayKeys[index].ID != id {
				continue
			}
			current := state.GatewayKeys[index]
			if payload.Name != nil {
				current.Name = strings.TrimSpace(*payload.Name)
			}
			if payload.Enabled != nil {
				current.Enabled = *payload.Enabled
			}
			if payload.ExpiresAt != nil {
				if bytes.Equal(bytes.TrimSpace(payload.ExpiresAt), []byte("null")) {
					current.ExpiresAt = nil
				} else {
					var expiresAt time.Time
					if err := json.Unmarshal(payload.ExpiresAt, &expiresAt); err != nil {
						return errValidation("expires_at must be a valid RFC3339 timestamp or null")
					}
					current.ExpiresAt = &expiresAt
				}
			}
			if payload.AllowedModels != nil {
				allowedModels, err := decodeOptionalStrings(payload.AllowedModels, "allowed_models")
				if err != nil {
					return errValidation(err.Error())
				}
				current.AllowedModels = allowedModels
			}
			if payload.AllowedProtocols != nil {
				allowedProtocols, err := decodeOptionalProtocols(payload.AllowedProtocols, "allowed_protocols")
				if err != nil {
					return errValidation(err.Error())
				}
				current.AllowedProtocols = allowedProtocols
			}
			if payload.MaxConcurrency != nil {
				current.MaxConcurrency = *payload.MaxConcurrency
			}
			if payload.RateLimitRPM != nil {
				current.RateLimitRPM = *payload.RateLimitRPM
			}
			if payload.HourlyBudgetUSD != nil {
				current.HourlyBudgetUSD = *payload.HourlyBudgetUSD
			}
			if payload.DailyBudgetUSD != nil {
				current.DailyBudgetUSD = *payload.DailyBudgetUSD
			}
			if payload.WeeklyBudgetUSD != nil {
				current.WeeklyBudgetUSD = *payload.WeeklyBudgetUSD
			}
			if payload.MonthlyBudgetUSD != nil {
				current.MonthlyBudgetUSD = *payload.MonthlyBudgetUSD
			}
			if payload.Notes != nil {
				current.Notes = *payload.Notes
			}
			if payload.Secret != nil {
				secret := strings.TrimSpace(*payload.Secret)
				if err := validateGatewaySecretStrength(secret); err != nil {
					return errValidation(err.Error())
				}
				hash, err := model.HashGatewaySecret(secret)
				if err != nil {
					return errValidation(err.Error())
				}
				current.SecretHash = hash
				current.SecretLookupHash = model.GatewaySecretLookupHash(secret, h.cfg.SecretLookupPepper())
				current.SecretPreview = model.SecretPreview(secret)
				rotatedSecret = secret
			}
			if strings.TrimSpace(current.Name) == "" {
				current.Name = current.ID
			}
			current.UpdatedAt = time.Now().UTC()
			state.GatewayKeys[index] = current
			return nil
		}
		return errNotFound("gateway key")
	})
	if err != nil {
		writeMutationError(w, err)
		return
	}
	key, _ := saved.FindGatewayKey(id)
	response := map[string]any{"gateway_key": key}
	if rotatedSecret != "" {
		response["secret"] = rotatedSecret
	}
	writeJSON(w, http.StatusOK, response)
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

// GetStatsSummary serves GET /api/admin/stats/summary.
func (h *Handlers) GetStatsSummary(w http.ResponseWriter, r *http.Request) {
	window, err := readStatsWindow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	report, err := h.store.StatsSummary(window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// GetStatsByGatewayKey serves GET /api/admin/stats/by-key.
func (h *Handlers) GetStatsByGatewayKey(w http.ResponseWriter, r *http.Request) {
	window, err := readStatsWindow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	report, err := h.store.StatsByGatewayKey(window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// GetStatsByProvider serves GET /api/admin/stats/by-provider.
func (h *Handlers) GetStatsByProvider(w http.ResponseWriter, r *http.Request) {
	window, err := readStatsWindow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	report, err := h.store.StatsByProvider(window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// GetStatsByModel serves GET /api/admin/stats/by-model.
func (h *Handlers) GetStatsByModel(w http.ResponseWriter, r *http.Request) {
	window, err := readStatsWindow(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	report, err := h.store.StatsByModel(window)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
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

// CheckinAllIntegrations executes manual check-ins for all enabled integrations
// that advertise check-in support.
func (h *Handlers) CheckinAllIntegrations(w http.ResponseWriter, r *http.Request) {
	result := h.RunCheckinsReport(r.Context(), false)
	failures, _ := result["failures"].([]map[string]any)
	if len(failures) == 0 {
		writeJSON(w, http.StatusOK, result)
		return
	}
	if successCount, _ := result["success_count"].(int); successCount == 0 {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusMultiStatus, result)
}

// SyncPricingCatalog executes a managed pricing catalog refresh.
func (h *Handlers) SyncPricingCatalog(w http.ResponseWriter, r *http.Request) {
	result := h.RunPricingSync(r.Context())
	if errValue, ok := result["error"].(string); ok && errValue != "" {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// RunCheckins executes background daily check-ins.
func (h *Handlers) RunCheckins(ctx context.Context) {
	_ = h.RunCheckinsReport(ctx, true)
}

// RunCheckinsReport executes automatic or manual check-ins and returns a
// detailed result summary suitable for admin maintenance endpoints.
func (h *Handlers) RunCheckinsReport(ctx context.Context, dueOnly bool) map[string]any {
	now := time.Now().UTC()
	snapshot := h.store.Snapshot()
	results := make([]integration.DailyCheckinResult, 0, len(snapshot.Integrations))
	for _, current := range snapshot.Integrations {
		if !current.Enabled || !current.Snapshot.SupportsCheckin {
			continue
		}
		if dueOnly && !integration.NeedsCheckin(current, now) {
			continue
		}
		results = append(results, integration.DailyCheckinResult{
			IntegrationID: current.ID,
			Result:        h.integration.PrepareCheckin(ctx, current),
		})
	}
	if len(results) == 0 {
		return map[string]any{
			"checked_at":     now,
			"results":        []any{},
			"success_count":  0,
			"failure_count":  0,
			"skipped_due_to": map[string]any{"mode": map[bool]string{true: "automatic", false: "manual"}[dueOnly]},
		}
	}

	failures := make([]map[string]any, 0)
	for _, result := range results {
		if result.Result.Err == nil {
			continue
		}
		failures = append(failures, map[string]any{
			"integration_id": result.IntegrationID,
			"error":          result.Result.Err.Error(),
		})
	}

	saved, err := h.store.Update(func(state *model.State) error {
		for _, result := range results {
			_ = h.integration.ApplyCheckinResult(state, result.IntegrationID, result.Result)
		}
		return nil
	})
	if err != nil {
		return map[string]any{
			"checked_at":    now,
			"results":       results,
			"success_count": 0,
			"failure_count": len(results),
			"error":         err.Error(),
		}
	}

	return map[string]any{
		"checked_at":    now,
		"results":       results,
		"success_count": len(results) - len(failures),
		"failure_count": len(failures),
		"failures":      failures,
		"state":         saved,
	}
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

// RunPricingSync refreshes managed pricing profiles from the configured public catalog.
func (h *Handlers) RunPricingSync(ctx context.Context) map[string]any {
	if h.pricing == nil || !h.pricing.Enabled() {
		return map[string]any{
			"checked_at": time.Now().UTC(),
			"profiles":   0,
			"error":      "pricing catalog is not configured",
		}
	}

	result := h.pricing.PrepareSync(ctx)
	if result.Err != nil {
		return map[string]any{
			"checked_at": result.FinishedAt,
			"source_url": result.SourceURL,
			"profiles":   len(result.Profiles),
			"error":      result.Err.Error(),
		}
	}

	saved, err := h.store.Update(func(state *model.State) error {
		pricing.ApplySyncResult(state, result)
		return nil
	})
	if err != nil {
		return map[string]any{
			"checked_at": result.FinishedAt,
			"source_url": result.SourceURL,
			"profiles":   len(result.Profiles),
			"error":      err.Error(),
		}
	}

	return map[string]any{
		"checked_at": result.FinishedAt,
		"source_url": result.SourceURL,
		"profiles":   len(result.Profiles),
		"state":      saved,
	}
}

func (h *Handlers) updateState(mutate func(*model.State) error) (model.State, error) {
	saved, err := h.store.Update(mutate)
	if err != nil {
		return model.State{}, err
	}
	return saved, nil
}

func mergeCompatibilityState(current, next *model.State) {
	current.Normalize()
	if next.Providers == nil {
		next.Providers = []model.Provider{}
	}
	if next.ModelRoutes == nil {
		next.ModelRoutes = []model.ModelRoute{}
	}
	if next.PricingProfiles == nil {
		next.PricingProfiles = []model.PricingProfile{}
	}
	if next.Integrations == nil {
		next.Integrations = []model.Integration{}
	}
	if next.GatewayKeys == nil {
		next.GatewayKeys = []model.GatewayKey{}
	}

	if len(next.GatewayKeys) == 0 {
		next.GatewayKeys = current.GatewayKeys
	} else {
		next.GatewayKeys = preserveGatewayKeys(current.GatewayKeys, next.GatewayKeys)
	}
	next.RequestHistory = current.RequestHistory
	for nextProviderIndex := range next.Providers {
		nextProvider := &next.Providers[nextProviderIndex]
		currentProvider, ok := current.FindProvider(nextProvider.ID)
		if !ok {
			continue
		}
		*nextProvider = preserveProviderSecrets(currentProvider, *nextProvider)
		if len(nextProvider.Endpoints) == 0 {
			nextProvider.Endpoints = currentProvider.Endpoints
		}
		if len(nextProvider.Credentials) == 0 {
			nextProvider.Credentials = currentProvider.Credentials
		}
		if strings.TrimSpace(nextProvider.ProxyURL) == "" {
			nextProvider.ProxyURL = currentProvider.ProxyURL
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
	for nextRouteIndex := range next.ModelRoutes {
		nextRoute := &next.ModelRoutes[nextRouteIndex]
		currentRoute, ok := current.FindRoute(nextRoute.Alias)
		if !ok {
			continue
		}
		if canonical := nextRoute.Strategy.Canonical(); canonical != "" {
			nextRoute.Strategy = canonical
		} else {
			nextRoute.Strategy = currentRoute.Strategy
		}
		if len(nextRoute.Scenarios) == 0 {
			nextRoute.Scenarios = currentRoute.Scenarios
		}
	}
	for nextIntegrationIndex := range next.Integrations {
		nextIntegration := &next.Integrations[nextIntegrationIndex]
		currentIntegration, ok := current.FindIntegration(nextIntegration.ID)
		if !ok {
			continue
		}
		*nextIntegration = preserveIntegrationSecrets(currentIntegration, *nextIntegration)
		if !nextIntegration.AutoSyncPricingProfiles {
			nextIntegration.AutoSyncPricingProfiles = currentIntegration.AutoSyncPricingProfiles
		}
	}
	next.Normalize()
}

func buildOpenAPISpec() map[string]any {
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Universal AI Gateway Admin API",
			"version":     "2026-04-01",
			"description": "RESTful administrative surface for providers, routes, pricing, integrations, gateway keys, request history, and maintenance operations.",
		},
		"paths": map[string]any{
			"/api/admin/state": map[string]any{
				"get": map[string]any{"summary": "Get full admin state snapshot"},
				"put": map[string]any{"summary": "Replace compatibility state snapshot"},
			},
			"/api/admin/meta": map[string]any{
				"get": map[string]any{"summary": "Get admin metadata"},
			},
			"/api/admin/integrations/sync": map[string]any{
				"post": map[string]any{"summary": "Sync all enabled integrations"},
			},
			"/api/admin/providers": map[string]any{
				"get":  map[string]any{"summary": "List providers"},
				"post": map[string]any{"summary": "Create provider"},
			},
			"/api/admin/providers/{id}": map[string]any{
				"get":    map[string]any{"summary": "Get one provider"},
				"put":    map[string]any{"summary": "Update one provider"},
				"delete": map[string]any{"summary": "Delete one provider"},
			},
			"/api/admin/routes": map[string]any{
				"get":  map[string]any{"summary": "List routes"},
				"post": map[string]any{"summary": "Create route"},
			},
			"/api/admin/routes/{alias}": map[string]any{
				"get":    map[string]any{"summary": "Get one route"},
				"put":    map[string]any{"summary": "Update one route"},
				"delete": map[string]any{"summary": "Delete one route"},
			},
			"/api/admin/pricing-profiles": map[string]any{
				"get":  map[string]any{"summary": "List pricing profiles"},
				"post": map[string]any{"summary": "Create pricing profile"},
			},
			"/api/admin/pricing-profiles/{id}": map[string]any{
				"put":    map[string]any{"summary": "Update one pricing profile"},
				"delete": map[string]any{"summary": "Delete one pricing profile"},
			},
			"/api/admin/integrations": map[string]any{
				"get":  map[string]any{"summary": "List integrations"},
				"post": map[string]any{"summary": "Create integration"},
			},
			"/api/admin/integrations/{id}": map[string]any{
				"put":    map[string]any{"summary": "Update one integration"},
				"delete": map[string]any{"summary": "Delete one integration"},
			},
			"/api/admin/integrations/{id}/sync": map[string]any{
				"post": map[string]any{"summary": "Sync one integration"},
			},
			"/api/admin/integrations/{id}/checkin": map[string]any{
				"post": map[string]any{"summary": "Run one integration daily checkin"},
			},
			"/api/admin/gateway-keys": map[string]any{
				"get":  map[string]any{"summary": "List gateway keys"},
				"post": map[string]any{"summary": "Create gateway key"},
			},
			"/api/admin/gateway-keys/{id}": map[string]any{
				"put":    map[string]any{"summary": "Update gateway key"},
				"delete": map[string]any{"summary": "Delete gateway key"},
			},
			"/api/admin/request-history": map[string]any{
				"get": map[string]any{"summary": "List request history"},
			},
			"/api/admin/request-history/{id}/attempts": map[string]any{
				"get": map[string]any{"summary": "Get recorded upstream attempts for one request"},
			},
			"/api/admin/stats/summary": map[string]any{
				"get": map[string]any{"summary": "Get aggregate request statistics"},
			},
			"/api/admin/stats/by-key": map[string]any{
				"get": map[string]any{"summary": "Get request statistics grouped by gateway key"},
			},
			"/api/admin/stats/by-provider": map[string]any{
				"get": map[string]any{"summary": "Get request statistics grouped by provider"},
			},
			"/api/admin/stats/by-model": map[string]any{
				"get": map[string]any{"summary": "Get request statistics grouped by routed model"},
			},
			"/api/admin/maintenance/probes": map[string]any{
				"post": map[string]any{"summary": "Probe all provider endpoints"},
			},
			"/api/admin/maintenance/checkins": map[string]any{
				"post": map[string]any{"summary": "Run check-ins for all enabled integrations that support daily checkin"},
			},
			"/api/admin/maintenance/pricing-sync": map[string]any{
				"post": map[string]any{"summary": "Refresh managed pricing profiles from the public catalog"},
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
	_ = json.NewEncoder(w).Encode(sanitizeAdminPayload(payload))
}

func sanitizeAdminPayload(payload any) any {
	switch current := payload.(type) {
	case model.State:
		return sanitizeState(current)
	case []model.Provider:
		items := make([]model.Provider, len(current))
		for i, provider := range current {
			items[i] = sanitizeProvider(provider)
		}
		return items
	case model.Provider:
		return sanitizeProvider(current)
	case []model.Integration:
		items := make([]model.Integration, len(current))
		for i, integration := range current {
			items[i] = sanitizeIntegration(integration)
		}
		return items
	case model.Integration:
		return sanitizeIntegration(current)
	case []model.GatewayKey:
		items := make([]model.GatewayKey, len(current))
		for i, key := range current {
			items[i] = sanitizeGatewayKey(key)
		}
		return items
	case model.GatewayKey:
		return sanitizeGatewayKey(current)
	case map[string]any:
		if state, ok := current["state"].(model.State); ok {
			next := cloneMap(current)
			next["state"] = sanitizeState(state)
			if gatewayKey, ok := next["gateway_key"].(model.GatewayKey); ok {
				next["gateway_key"] = sanitizeGatewayKey(gatewayKey)
			}
			return next
		}
		if gatewayKey, ok := current["gateway_key"].(model.GatewayKey); ok {
			next := cloneMap(current)
			next["gateway_key"] = sanitizeGatewayKey(gatewayKey)
			return next
		}
		return current
	default:
		return payload
	}
}

func sanitizeState(state model.State) model.State {
	state = state.Clone()
	for index := range state.Providers {
		state.Providers[index] = sanitizeProvider(state.Providers[index])
	}
	for index := range state.Integrations {
		state.Integrations[index] = sanitizeIntegration(state.Integrations[index])
	}
	for index := range state.GatewayKeys {
		state.GatewayKeys[index] = sanitizeGatewayKey(state.GatewayKeys[index])
	}
	return state
}

func sanitizeProvider(provider model.Provider) model.Provider {
	providerCopy := provider
	providerCopy.APIKeyPreview = model.SecretPreview(providerCopy.APIKey)
	providerCopy.APIKey = ""
	providerCopy.Credentials = append([]model.ProviderCredential(nil), provider.Credentials...)
	for index := range providerCopy.Credentials {
		providerCopy.Credentials[index].APIKeyPreview = model.SecretPreview(providerCopy.Credentials[index].APIKey)
		providerCopy.Credentials[index].APIKey = ""
	}
	return providerCopy
}

func sanitizeIntegration(integration model.Integration) model.Integration {
	integrationCopy := integration
	integrationCopy.AccessKeyPreview = model.SecretPreview(integrationCopy.AccessKey)
	integrationCopy.AccessKey = ""
	integrationCopy.RelayAPIKeyPreview = model.SecretPreview(integrationCopy.RelayAPIKey)
	integrationCopy.RelayAPIKey = ""
	return integrationCopy
}

func sanitizeGatewayKey(key model.GatewayKey) model.GatewayKey {
	key.SecretHash = ""
	key.SecretLookupHash = ""
	return key
}

func preserveProviderSecrets(current, next model.Provider) model.Provider {
	if strings.TrimSpace(next.APIKey) == "" {
		next.APIKey = current.APIKey
	}
	if strings.TrimSpace(next.ProxyURL) == "" {
		next.ProxyURL = current.ProxyURL
	}
	if len(next.Endpoints) == 0 {
		next.Endpoints = current.Endpoints
	}
	if len(next.Credentials) == 0 {
		next.Credentials = current.Credentials
		return next
	}

	byID := make(map[string]model.ProviderCredential, len(current.Credentials))
	for _, credential := range current.Credentials {
		byID[credential.ID] = credential
	}
	for index := range next.Credentials {
		credential := &next.Credentials[index]
		currentCredential, ok := byID[credential.ID]
		if ok && strings.TrimSpace(credential.APIKey) == "" {
			credential.APIKey = currentCredential.APIKey
		}
	}
	return next
}

func preserveIntegrationSecrets(current, next model.Integration) model.Integration {
	if strings.TrimSpace(next.AccessKey) == "" {
		next.AccessKey = current.AccessKey
	}
	if strings.TrimSpace(next.RelayAPIKey) == "" {
		next.RelayAPIKey = current.RelayAPIKey
	}
	return next
}

func preserveGatewayKeys(current, next []model.GatewayKey) []model.GatewayKey {
	byID := make(map[string]model.GatewayKey, len(current))
	for _, key := range current {
		byID[key.ID] = key
	}
	preserved := make([]model.GatewayKey, len(next))
	for index := range next {
		key := next[index]
		currentKey, ok := byID[key.ID]
		if ok {
			key = preserveGatewayKeySecrets(currentKey, key)
		}
		preserved[index] = key
	}
	return preserved
}

func preserveGatewayKeySecrets(current, next model.GatewayKey) model.GatewayKey {
	next.SecretHash = current.SecretHash
	next.SecretLookupHash = current.SecretLookupHash
	next.SecretPreview = current.SecretPreview
	if next.ExpiresAt == nil {
		next.ExpiresAt = cloneTimeValue(current.ExpiresAt)
	}
	if next.LastUsedAt == nil {
		next.LastUsedAt = cloneTimeValue(current.LastUsedAt)
	}
	if next.CreatedAt.IsZero() {
		next.CreatedAt = current.CreatedAt
	}
	if next.UpdatedAt.IsZero() {
		next.UpdatedAt = current.UpdatedAt
	}
	return next
}

func cloneTimeValue(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func decodeOptionalStrings(raw json.RawMessage, field string) ([]string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s must be an array of strings or null", field)
	}
	return values, nil
}

func decodeOptionalProtocols(raw json.RawMessage, field string) ([]model.Protocol, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values []model.Protocol
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s must be an array of protocol identifiers or null", field)
	}
	return values, nil
}

func validateGatewaySecretStrength(secret string) error {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return errors.New("custom gateway secret is required")
	}
	if len(trimmed) < 16 {
		return errors.New("custom gateway secrets must be at least 16 characters long")
	}
	if len([]byte(trimmed)) > 72 {
		return errors.New("custom gateway secrets must be 72 bytes or fewer")
	}
	return nil
}

func validateRouteStrategy(route *model.ModelRoute) bool {
	if route == nil {
		return false
	}
	if route.Strategy != "" {
		canonical := route.Strategy.Canonical()
		if canonical == "" {
			return false
		}
		route.Strategy = canonical
	}
	for index := range route.Scenarios {
		scenario := &route.Scenarios[index]
		scenario.Name = strings.TrimSpace(scenario.Name)
		if scenario.Name == "" {
			return false
		}
		if scenario.Strategy == "" {
			continue
		}
		canonical := scenario.Strategy.Canonical()
		if canonical == "" {
			return false
		}
		scenario.Strategy = canonical
	}
	for index := range route.Transformers {
		transformer := &route.Transformers[index]
		transformer.Name = strings.TrimSpace(transformer.Name)
		transformer.Target = strings.TrimSpace(transformer.Target)
		transformer.Phase = transformer.Phase.Canonical()
		transformer.Type = transformer.Type.Canonical()
		if transformer.Phase == "" || transformer.Type == "" {
			return false
		}
		switch transformer.Type {
		case model.TransformerTypeSetHeader, model.TransformerTypeRemoveHeader, model.TransformerTypeSetJSON, model.TransformerTypeDeleteJSON:
			if transformer.Target == "" {
				return false
			}
		case model.TransformerTypePrependMessage:
			if transformer.Phase != model.TransformerPhaseRequest {
				return false
			}
		}
	}
	return true
}

func validateProviderProxyURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("proxy_url must be a valid URL: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("proxy_url must use http, https, socks5, or socks5h")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return errors.New("proxy_url host is required")
	}
	return nil
}

func cloneMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	next := make(map[string]any, len(values))
	for key, value := range values {
		next[key] = value
	}
	return next
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	reader := http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	defer reader.Close()

	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain a single JSON value")
		}
		return err
	}
	return nil
}

func readStatsWindow(r *http.Request) (store.StatsWindow, error) {
	query := r.URL.Query()
	return store.ResolveStatsWindow(query.Get("window"), query.Get("days"), time.Now().UTC())
}

func bearerToken(value string) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

type resourceError struct {
	name string
}

type validationError struct {
	message string
}

func (e resourceError) Error() string {
	return e.name + " not found"
}

func errNotFound(name string) error {
	return resourceError{name: name}
}

func errValidation(message string) error {
	return validationError{message: message}
}

func writeResourceError(w http.ResponseWriter, err error) {
	var resource resourceError
	if ok := errors.As(err, &resource); ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

func (e validationError) Error() string {
	return e.message
}

func writeMutationError(w http.ResponseWriter, err error) {
	var validation validationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": validation.Error()})
		return
	}
	writeResourceError(w, err)
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

func validateRouteAlias(routes []model.ModelRoute, alias, currentAlias string) error {
	trimmedAlias := strings.TrimSpace(alias)
	if trimmedAlias == "" {
		return errValidation("route alias is required")
	}
	for _, route := range routes {
		if !strings.EqualFold(route.Alias, trimmedAlias) {
			continue
		}
		if currentAlias != "" && strings.EqualFold(route.Alias, currentAlias) {
			continue
		}
		return errValidation("route alias already exists")
	}
	return nil
}

func removeGatewayKeyModelReferences(keys []model.GatewayKey, aliases map[string]struct{}) {
	if len(aliases) == 0 {
		return
	}
	updatedAt := time.Now().UTC()
	for keyIndex := range keys {
		if len(keys[keyIndex].AllowedModels) == 0 {
			continue
		}
		original := keys[keyIndex].AllowedModels
		filtered := make([]string, 0, len(original))
		removedAny := false
		for _, alias := range original {
			if _, removed := aliases[strings.ToLower(strings.TrimSpace(alias))]; removed {
				removedAny = true
				continue
			}
			filtered = append(filtered, alias)
		}
		if removedAny {
			keys[keyIndex].AllowedModels = slices.Clone(filtered)
			keys[keyIndex].UpdatedAt = updatedAt
		}
	}
}

func removeProviderReferences(state *model.State, providerID string) {
	if state == nil || strings.TrimSpace(providerID) == "" {
		return
	}
	removedAliases := map[string]struct{}{}
	filteredRoutes := state.ModelRoutes[:0]
	for _, route := range state.ModelRoutes {
		filteredTargets := route.Targets[:0]
		for _, target := range route.Targets {
			if target.AccountID != providerID {
				filteredTargets = append(filteredTargets, target)
			}
		}
		route.Targets = filteredTargets
		filteredScenarios := route.Scenarios[:0]
		for _, scenario := range route.Scenarios {
			scenarioTargets := scenario.Targets[:0]
			for _, target := range scenario.Targets {
				if target.AccountID != providerID {
					scenarioTargets = append(scenarioTargets, target)
				}
			}
			scenario.Targets = scenarioTargets
			filteredScenarios = append(filteredScenarios, scenario)
		}
		route.Scenarios = filteredScenarios
		routeHasScenarioTargets := false
		for _, scenario := range route.Scenarios {
			if len(scenario.Targets) > 0 {
				routeHasScenarioTargets = true
				break
			}
		}
		if len(route.Targets) == 0 && !routeHasScenarioTargets {
			removedAliases[strings.ToLower(strings.TrimSpace(route.Alias))] = struct{}{}
			continue
		}
		filteredRoutes = append(filteredRoutes, route)
	}
	state.ModelRoutes = filteredRoutes
	removeGatewayKeyModelReferences(state.GatewayKeys, removedAliases)
}

func removeIntegrationPricingProfiles(state *model.State, integrationID string) {
	if state == nil {
		return
	}
	prefix := "pricing-sync-" + strings.TrimSpace(integrationID) + "-"
	if strings.TrimSpace(prefix) == "pricing-sync--" {
		return
	}
	filtered := state.PricingProfiles[:0]
	removedProfiles := map[string]struct{}{}
	for _, profile := range state.PricingProfiles {
		if strings.HasPrefix(profile.ID, prefix) {
			removedProfiles[profile.ID] = struct{}{}
			continue
		}
		filtered = append(filtered, profile)
	}
	state.PricingProfiles = filtered
	if len(removedProfiles) == 0 {
		return
	}
	for routeIndex := range state.ModelRoutes {
		if _, removed := removedProfiles[state.ModelRoutes[routeIndex].PricingProfileID]; removed {
			state.ModelRoutes[routeIndex].PricingProfileID = ""
		}
	}
}
