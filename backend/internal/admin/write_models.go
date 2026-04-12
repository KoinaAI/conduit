package admin

import (
	"maps"
	"slices"
	"strings"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

type stateWriteRequest struct {
	Version         string                    `json:"version"`
	Providers       []providerWriteRequest    `json:"providers"`
	ModelRoutes     []model.ModelRoute        `json:"model_routes"`
	PricingProfiles []model.PricingProfile    `json:"pricing_profiles"`
	Integrations    []integrationWriteRequest `json:"integrations"`
}

func (r stateWriteRequest) toModelState() model.State {
	state := model.State{
		Version:         strings.TrimSpace(r.Version),
		Providers:       make([]model.Provider, len(r.Providers)),
		ModelRoutes:     slices.Clone(r.ModelRoutes),
		PricingProfiles: slices.Clone(r.PricingProfiles),
		Integrations:    make([]model.Integration, len(r.Integrations)),
	}
	for i, provider := range r.Providers {
		state.Providers[i] = provider.toModel()
	}
	for i, integration := range r.Integrations {
		state.Integrations[i] = integration.toModel()
	}
	return state
}

type providerWriteRequest struct {
	ID                      string                         `json:"id"`
	Name                    string                         `json:"name"`
	Kind                    model.ProviderKind             `json:"kind"`
	Enabled                 bool                           `json:"enabled"`
	Weight                  int                            `json:"weight"`
	TimeoutSeconds          int                            `json:"timeout_seconds"`
	DefaultMarkupMultiplier float64                        `json:"default_markup_multiplier"`
	Capabilities            []model.Protocol               `json:"capabilities"`
	Headers                 map[string]string              `json:"headers,omitempty"`
	Notes                   string                         `json:"notes,omitempty"`
	RoutingMode             model.ProviderRoutingMode      `json:"routing_mode,omitempty"`
	MaxAttempts             int                            `json:"max_attempts,omitempty"`
	StickySessionTTLSeconds int                            `json:"sticky_session_ttl_seconds,omitempty"`
	HealthcheckPath         string                         `json:"healthcheck_path,omitempty"`
	Endpoints               []providerEndpointWriteInput   `json:"endpoints,omitempty"`
	Credentials             []providerCredentialWriteInput `json:"credentials,omitempty"`
	CircuitBreaker          model.CircuitBreakerConfig     `json:"circuit_breaker,omitempty"`
}

func (r providerWriteRequest) toModel() model.Provider {
	provider := model.Provider{
		ID:                      strings.TrimSpace(r.ID),
		Name:                    strings.TrimSpace(r.Name),
		Kind:                    r.Kind,
		Enabled:                 r.Enabled,
		Weight:                  r.Weight,
		TimeoutSeconds:          r.TimeoutSeconds,
		DefaultMarkupMultiplier: r.DefaultMarkupMultiplier,
		Capabilities:            slices.Clone(r.Capabilities),
		Headers:                 maps.Clone(r.Headers),
		Notes:                   r.Notes,
		RoutingMode:             r.RoutingMode,
		MaxAttempts:             r.MaxAttempts,
		StickySessionTTLSeconds: r.StickySessionTTLSeconds,
		HealthcheckPath:         strings.TrimSpace(r.HealthcheckPath),
		CircuitBreaker:          r.CircuitBreaker,
		Endpoints:               make([]model.ProviderEndpoint, len(r.Endpoints)),
		Credentials:             make([]model.ProviderCredential, len(r.Credentials)),
	}
	for i, endpoint := range r.Endpoints {
		provider.Endpoints[i] = endpoint.toModel()
	}
	for i, credential := range r.Credentials {
		provider.Credentials[i] = credential.toModel()
	}
	return provider
}

type providerEndpointWriteInput struct {
	ID              string            `json:"id"`
	Label           string            `json:"label,omitempty"`
	BaseURL         string            `json:"base_url"`
	Enabled         bool              `json:"enabled"`
	Weight          int               `json:"weight"`
	Priority        int               `json:"priority"`
	HealthcheckPath string            `json:"healthcheck_path,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Notes           string            `json:"notes,omitempty"`
}

func (r providerEndpointWriteInput) toModel() model.ProviderEndpoint {
	return model.ProviderEndpoint{
		ID:              strings.TrimSpace(r.ID),
		Label:           strings.TrimSpace(r.Label),
		BaseURL:         strings.TrimSpace(r.BaseURL),
		Enabled:         r.Enabled,
		Weight:          r.Weight,
		Priority:        r.Priority,
		HealthcheckPath: strings.TrimSpace(r.HealthcheckPath),
		Headers:         maps.Clone(r.Headers),
		Notes:           r.Notes,
	}
}

type providerCredentialWriteInput struct {
	ID      string            `json:"id"`
	Label   string            `json:"label,omitempty"`
	APIKey  string            `json:"api_key"`
	Enabled bool              `json:"enabled"`
	Weight  int               `json:"weight"`
	Headers map[string]string `json:"headers,omitempty"`
	Notes   string            `json:"notes,omitempty"`
}

func (r providerCredentialWriteInput) toModel() model.ProviderCredential {
	return model.ProviderCredential{
		ID:      strings.TrimSpace(r.ID),
		Label:   strings.TrimSpace(r.Label),
		APIKey:  strings.TrimSpace(r.APIKey),
		Enabled: r.Enabled,
		Weight:  r.Weight,
		Headers: maps.Clone(r.Headers),
		Notes:   r.Notes,
	}
}

type integrationWriteRequest struct {
	ID                      string                `json:"id"`
	Name                    string                `json:"name"`
	Kind                    model.IntegrationKind `json:"kind"`
	BaseURL                 string                `json:"base_url"`
	UserID                  string                `json:"user_id,omitempty"`
	AccessKey               string                `json:"access_key"`
	RelayAPIKey             string                `json:"relay_api_key,omitempty"`
	Enabled                 bool                  `json:"enabled"`
	LinkedProviderID        string                `json:"linked_provider_id,omitempty"`
	AutoCreateRoutes        bool                  `json:"auto_create_routes"`
	DefaultProtocols        []model.Protocol      `json:"default_protocols"`
	DefaultMarkupMultiplier float64               `json:"default_markup_multiplier"`
	ModelMarkupOverrides    map[string]float64    `json:"model_markup_overrides,omitempty"`
}

func (r integrationWriteRequest) toModel() model.Integration {
	return model.Integration{
		ID:                      strings.TrimSpace(r.ID),
		Name:                    strings.TrimSpace(r.Name),
		Kind:                    r.Kind,
		BaseURL:                 strings.TrimSpace(r.BaseURL),
		UserID:                  strings.TrimSpace(r.UserID),
		AccessKey:               strings.TrimSpace(r.AccessKey),
		RelayAPIKey:             strings.TrimSpace(r.RelayAPIKey),
		Enabled:                 r.Enabled,
		LinkedProviderID:        strings.TrimSpace(r.LinkedProviderID),
		AutoCreateRoutes:        r.AutoCreateRoutes,
		DefaultProtocols:        slices.Clone(r.DefaultProtocols),
		DefaultMarkupMultiplier: r.DefaultMarkupMultiplier,
		ModelMarkupOverrides:    maps.Clone(r.ModelMarkupOverrides),
	}
}
