package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
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

const maxAdminBodyBytes = 1 << 20

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
		adminToken := strings.TrimSpace(h.cfg.AdminToken)
		if adminToken == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "admin API is not configured"})
			return
		}
		token := r.Header.Get("X-Admin-Token")
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
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
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
			if firstErr == nil && result.Err != nil {
				firstErr = result.Err
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": firstErr.Error()})
		return
	}

	saved, err := h.updateState(func(state *model.State) error {
		for _, operation := range operations {
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
	writeJSON(w, http.StatusOK, saved)
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
		removedAliases := map[string]struct{}{}
		filteredRoutes := state.ModelRoutes[:0]
		for _, route := range state.ModelRoutes {
			filteredTargets := route.Targets[:0]
			for _, target := range route.Targets {
				if target.AccountID != id {
					filteredTargets = append(filteredTargets, target)
				}
			}
			route.Targets = filteredTargets
			if len(route.Targets) == 0 {
				removedAliases[strings.ToLower(strings.TrimSpace(route.Alias))] = struct{}{}
				continue
			}
			filteredRoutes = append(filteredRoutes, route)
		}
		state.ModelRoutes = filteredRoutes
		removeGatewayKeyModelReferences(state.GatewayKeys, removedAliases)
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
	saved, err := h.updateState(func(state *model.State) error {
		if err := validateRouteAlias(state.ModelRoutes, payload.Alias, alias); err != nil {
			return err
		}
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

// UpdateGatewayKey serves PUT /api/admin/gateway-keys/{id}.
func (h *Handlers) UpdateGatewayKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var payload struct {
		Name             *string          `json:"name"`
		Secret           *string          `json:"secret"`
		Enabled          *bool            `json:"enabled"`
		ExpiresAt        json.RawMessage  `json:"expires_at"`
		AllowedModels    []string         `json:"allowed_models"`
		AllowedProtocols []model.Protocol `json:"allowed_protocols"`
		MaxConcurrency   *int             `json:"max_concurrency"`
		RateLimitRPM     *int             `json:"rate_limit_rpm"`
		DailyBudgetUSD   *float64         `json:"daily_budget_usd"`
		Notes            *string          `json:"notes"`
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
				current.AllowedModels = payload.AllowedModels
			}
			if payload.AllowedProtocols != nil {
				current.AllowedProtocols = payload.AllowedProtocols
			}
			if payload.MaxConcurrency != nil {
				current.MaxConcurrency = *payload.MaxConcurrency
			}
			if payload.RateLimitRPM != nil {
				current.RateLimitRPM = *payload.RateLimitRPM
			}
			if payload.DailyBudgetUSD != nil {
				current.DailyBudgetUSD = *payload.DailyBudgetUSD
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
	for nextIntegrationIndex := range next.Integrations {
		nextIntegration := &next.Integrations[nextIntegrationIndex]
		currentIntegration, ok := current.FindIntegration(nextIntegration.ID)
		if !ok {
			continue
		}
		*nextIntegration = preserveIntegrationSecrets(currentIntegration, *nextIntegration)
	}
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
			"/api/admin/gateway-keys/{id}": map[string]any{
				"put":    map[string]any{"summary": "Update gateway key"},
				"delete": map[string]any{"summary": "Delete gateway key"},
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
	key.SecretLookupHash = ""
	return key
}

func preserveProviderSecrets(current, next model.Provider) model.Provider {
	if strings.TrimSpace(next.APIKey) == "" {
		next.APIKey = current.APIKey
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
		if !ok && index < len(current.Credentials) {
			currentCredential = current.Credentials[index]
			ok = true
		}
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
		filtered := original[:0]
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
