package model

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// Protocol identifies the public-facing gateway API surface.
type Protocol string

const (
	ProtocolOpenAIChat      Protocol = "openai.chat"
	ProtocolOpenAIResponses Protocol = "openai.responses"
	ProtocolOpenAIRealtime  Protocol = "openai.realtime"
	ProtocolAnthropic       Protocol = "anthropic.messages"
	ProtocolGeminiGenerate  Protocol = "gemini.generate"
	ProtocolGeminiStream    Protocol = "gemini.stream"
)

// ProviderKind describes the native upstream format that a provider speaks.
type ProviderKind string

const (
	ProviderKindOpenAICompatible ProviderKind = "openai-compatible"
	ProviderKindAnthropic        ProviderKind = "anthropic"
	ProviderKindGemini           ProviderKind = "gemini"
)

// IntegrationKind identifies supported relay-management integrations.
type IntegrationKind string

const (
	IntegrationKindNewAPI IntegrationKind = "newapi"
	IntegrationKindOneHub IntegrationKind = "onehub"
)

// ProviderRoutingMode controls how candidate endpoints are ranked before
// request-level retry and failover are applied.
type ProviderRoutingMode string

const (
	ProviderRoutingModeWeighted ProviderRoutingMode = "weighted"
	ProviderRoutingModeLatency  ProviderRoutingMode = "latency"
)

// State is the persisted configuration snapshot exposed to the admin console.
//
// It intentionally keeps the original top-level shape so the current frontend
// can continue to load and save a compatibility snapshot while the backend
// grows richer internal behavior.
type State struct {
	Version         string           `json:"version"`
	Providers       []Provider       `json:"providers"`
	ModelRoutes     []ModelRoute     `json:"model_routes"`
	PricingProfiles []PricingProfile `json:"pricing_profiles"`
	Integrations    []Integration    `json:"integrations"`
	GatewayKeys     []GatewayKey     `json:"gateway_keys"`
	RequestHistory  []RequestRecord  `json:"request_history"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// RoutingState is the read-only subset needed on the hot path.
type RoutingState struct {
	Providers       []Provider
	ModelRoutes     []ModelRoute
	PricingProfiles []PricingProfile
	GatewayKeys     []GatewayKey
}

// Provider defines one upstream provider inventory item. Compatibility fields
// `base_url` and `api_key` are mirrored from the first endpoint/credential so
// the existing frontend remains functional until it is refactored.
type Provider struct {
	ID                      string               `json:"id"`
	Name                    string               `json:"name"`
	Kind                    ProviderKind         `json:"kind"`
	BaseURL                 string               `json:"base_url"`
	APIKey                  string               `json:"api_key"`
	APIKeyPreview           string               `json:"api_key_preview,omitempty"`
	Enabled                 bool                 `json:"enabled"`
	Weight                  int                  `json:"weight"`
	TimeoutSeconds          int                  `json:"timeout_seconds"`
	DefaultMarkupMultiplier float64              `json:"default_markup_multiplier"`
	Capabilities            []Protocol           `json:"capabilities"`
	Headers                 map[string]string    `json:"headers,omitempty"`
	Notes                   string               `json:"notes,omitempty"`
	RoutingMode             ProviderRoutingMode  `json:"routing_mode,omitempty"`
	MaxAttempts             int                  `json:"max_attempts,omitempty"`
	StickySessionTTLSeconds int                  `json:"sticky_session_ttl_seconds,omitempty"`
	HealthcheckPath         string               `json:"healthcheck_path,omitempty"`
	Endpoints               []ProviderEndpoint   `json:"endpoints,omitempty"`
	Credentials             []ProviderCredential `json:"credentials,omitempty"`
	CircuitBreaker          CircuitBreakerConfig `json:"circuit_breaker,omitempty"`
	CreatedAt               time.Time            `json:"created_at"`
	UpdatedAt               time.Time            `json:"updated_at"`
}

// ProviderEndpoint stores one concrete upstream base URL.
type ProviderEndpoint struct {
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

// ProviderCredential stores one concrete upstream credential. Keeping multiple
// credentials under one provider enables key cooldown and request-level
// failover without duplicating the whole provider definition.
type ProviderCredential struct {
	ID            string            `json:"id"`
	Label         string            `json:"label,omitempty"`
	APIKey        string            `json:"api_key"`
	APIKeyPreview string            `json:"api_key_preview,omitempty"`
	Enabled       bool              `json:"enabled"`
	Weight        int               `json:"weight"`
	Headers       map[string]string `json:"headers,omitempty"`
	Notes         string            `json:"notes,omitempty"`
}

// CircuitBreakerConfig controls passive endpoint protection on the hot path.
type CircuitBreakerConfig struct {
	Enabled             *bool `json:"enabled,omitempty"`
	FailureThreshold    int   `json:"failure_threshold"`
	CooldownSeconds     int   `json:"cooldown_seconds"`
	HalfOpenMaxRequests int   `json:"half_open_max_requests"`
}

// ModelRoute maps one public alias to one or more upstream targets.
type ModelRoute struct {
	Alias            string        `json:"alias"`
	PricingProfileID string        `json:"pricing_profile_id,omitempty"`
	Targets          []RouteTarget `json:"targets"`
	Notes            string        `json:"notes,omitempty"`
}

// RouteTarget points at a provider inventory item.
type RouteTarget struct {
	ID               string     `json:"id"`
	AccountID        string     `json:"account_id"`
	UpstreamModel    string     `json:"upstream_model"`
	Weight           int        `json:"weight"`
	Priority         int        `json:"priority,omitempty"`
	Enabled          bool       `json:"enabled"`
	MarkupMultiplier float64    `json:"markup_multiplier"`
	Protocols        []Protocol `json:"protocols,omitempty"`
}

// PricingProfile defines local billing rates.
type PricingProfile struct {
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Currency              string  `json:"currency"`
	InputPerMillion       float64 `json:"input_per_million"`
	OutputPerMillion      float64 `json:"output_per_million"`
	CachedInputPerMillion float64 `json:"cached_input_per_million"`
	ReasoningPerMillion   float64 `json:"reasoning_per_million"`
	RequestFlat           float64 `json:"request_flat"`
}

// Integration tracks a managed upstream relay account.
type Integration struct {
	ID                      string              `json:"id"`
	Name                    string              `json:"name"`
	Kind                    IntegrationKind     `json:"kind"`
	BaseURL                 string              `json:"base_url"`
	UserID                  string              `json:"user_id,omitempty"`
	AccessKey               string              `json:"access_key"`
	AccessKeyPreview        string              `json:"access_key_preview,omitempty"`
	RelayAPIKey             string              `json:"relay_api_key,omitempty"`
	RelayAPIKeyPreview      string              `json:"relay_api_key_preview,omitempty"`
	Enabled                 bool                `json:"enabled"`
	LinkedProviderID        string              `json:"linked_provider_id,omitempty"`
	AutoCreateRoutes        bool                `json:"auto_create_routes"`
	DefaultProtocols        []Protocol          `json:"default_protocols"`
	DefaultMarkupMultiplier float64             `json:"default_markup_multiplier"`
	ModelMarkupOverrides    map[string]float64  `json:"model_markup_overrides,omitempty"`
	Snapshot                IntegrationSnapshot `json:"snapshot"`
	UpdatedAt               time.Time           `json:"updated_at"`
}

// IntegrationSnapshot caches the latest remote sync result.
type IntegrationSnapshot struct {
	Balance           float64                       `json:"balance"`
	Used              float64                       `json:"used"`
	Currency          string                        `json:"currency"`
	ModelNames        []string                      `json:"model_names"`
	Prices            map[string]IntegrationPricing `json:"prices,omitempty"`
	SupportsCheckin   bool                          `json:"supports_checkin"`
	LastCheckinAt     *time.Time                    `json:"last_checkin_at,omitempty"`
	LastCheckinResult string                        `json:"last_checkin_result,omitempty"`
	LastSyncAt        *time.Time                    `json:"last_sync_at,omitempty"`
	LastError         string                        `json:"last_error,omitempty"`
}

// IntegrationPricing is imported from relay sites when available.
type IntegrationPricing struct {
	InputPerMillion  float64 `json:"input_per_million"`
	OutputPerMillion float64 `json:"output_per_million"`
	Currency         string  `json:"currency"`
}

// GatewayKey is the consumer-facing credential used to access /v1/* routes.
type GatewayKey struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	SecretHash       string     `json:"secret_hash,omitempty"`
	SecretLookupHash string     `json:"secret_lookup_hash,omitempty"`
	SecretPreview    string     `json:"secret_preview"`
	Enabled          bool       `json:"enabled"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	AllowedModels    []string   `json:"allowed_models,omitempty"`
	AllowedProtocols []Protocol `json:"allowed_protocols,omitempty"`
	MaxConcurrency   int        `json:"max_concurrency,omitempty"`
	RateLimitRPM     int        `json:"rate_limit_rpm,omitempty"`
	DailyBudgetUSD   float64    `json:"daily_budget_usd,omitempty"`
	Notes            string     `json:"notes,omitempty"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// UsageSummary stores normalized token counters across different protocols.
type UsageSummary struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
}

// BillingSummary stores the local billing result derived from usage.
type BillingSummary struct {
	Currency   string  `json:"currency"`
	BaseCost   float64 `json:"base_cost"`
	FinalCost  float64 `json:"final_cost"`
	Multiplier float64 `json:"multiplier"`
	Source     string  `json:"source"`
}

// RequestRecord is the top-level request history entry shown in the current UI.
type RequestRecord struct {
	ID              string         `json:"id"`
	Protocol        Protocol       `json:"protocol"`
	RouteAlias      string         `json:"route_alias"`
	AccountID       string         `json:"account_id"`
	ProviderName    string         `json:"provider_name"`
	UpstreamModel   string         `json:"upstream_model"`
	GatewayKeyID    string         `json:"gateway_key_id,omitempty"`
	ClientSessionID string         `json:"client_session_id,omitempty"`
	Path            string         `json:"path,omitempty"`
	StatusCode      int            `json:"status_code"`
	Stream          bool           `json:"stream"`
	AttemptCount    int            `json:"attempt_count"`
	StartedAt       time.Time      `json:"started_at"`
	DurationMS      int64          `json:"duration_ms"`
	Usage           UsageSummary   `json:"usage"`
	Billing         BillingSummary `json:"billing"`
	Error           string         `json:"error,omitempty"`
}

// RequestAttemptRecord stores one concrete upstream attempt for a request.
type RequestAttemptRecord struct {
	RequestID     string    `json:"request_id"`
	Sequence      int       `json:"sequence"`
	ProviderID    string    `json:"provider_id"`
	ProviderName  string    `json:"provider_name"`
	EndpointID    string    `json:"endpoint_id"`
	EndpointURL   string    `json:"endpoint_url"`
	CredentialID  string    `json:"credential_id"`
	UpstreamModel string    `json:"upstream_model"`
	StatusCode    int       `json:"status_code"`
	Retryable     bool      `json:"retryable"`
	Decision      string    `json:"decision"`
	StartedAt     time.Time `json:"started_at"`
	DurationMS    int64     `json:"duration_ms"`
	Error         string    `json:"error,omitempty"`
}

// DefaultState returns an empty but fully initialized configuration snapshot.
func DefaultState() State {
	return State{
		Version:         "2026-04-01",
		Providers:       []Provider{},
		ModelRoutes:     []ModelRoute{},
		PricingProfiles: []PricingProfile{},
		Integrations:    []Integration{},
		GatewayKeys:     []GatewayKey{},
		RequestHistory:  []RequestRecord{},
		UpdatedAt:       time.Now().UTC(),
	}
}

// Normalize fills defaults and backfills compatibility fields.
func (s *State) Normalize() {
	if s.Version == "" {
		s.Version = "2026-04-01"
	}
	if s.Providers == nil {
		s.Providers = []Provider{}
	}
	if s.ModelRoutes == nil {
		s.ModelRoutes = []ModelRoute{}
	}
	if s.PricingProfiles == nil {
		s.PricingProfiles = []PricingProfile{}
	}
	if s.Integrations == nil {
		s.Integrations = []Integration{}
	}
	if s.GatewayKeys == nil {
		s.GatewayKeys = []GatewayKey{}
	}
	if s.RequestHistory == nil {
		s.RequestHistory = []RequestRecord{}
	}

	for i := range s.Providers {
		p := &s.Providers[i]
		if p.ID == "" {
			p.ID = NewID("provider")
		}
		if p.Weight <= 0 {
			p.Weight = 1
		}
		if p.TimeoutSeconds <= 0 {
			p.TimeoutSeconds = 180
		}
		if p.DefaultMarkupMultiplier <= 0 {
			p.DefaultMarkupMultiplier = 1
		}
		if p.MaxAttempts <= 0 {
			p.MaxAttempts = 3
		}
		if p.StickySessionTTLSeconds <= 0 {
			p.StickySessionTTLSeconds = 300
		}
		if p.RoutingMode == "" {
			p.RoutingMode = ProviderRoutingModeWeighted
		}
		if p.Headers == nil {
			p.Headers = map[string]string{}
		}
		if p.Capabilities == nil {
			p.Capabilities = []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses}
		}
		if p.CircuitBreaker.FailureThreshold <= 0 {
			p.CircuitBreaker.FailureThreshold = 3
		}
		if p.CircuitBreaker.CooldownSeconds <= 0 {
			p.CircuitBreaker.CooldownSeconds = 60
		}
		if p.CircuitBreaker.HalfOpenMaxRequests <= 0 {
			p.CircuitBreaker.HalfOpenMaxRequests = 1
		}
		if p.CircuitBreaker.Enabled == nil {
			p.CircuitBreaker.Enabled = boolPointer(true)
		}

		if len(p.Endpoints) == 0 && strings.TrimSpace(p.BaseURL) != "" {
			p.Endpoints = []ProviderEndpoint{{
				ID:       NewID("endpoint"),
				Label:    "default",
				BaseURL:  strings.TrimSpace(p.BaseURL),
				Enabled:  true,
				Weight:   1,
				Priority: 0,
				Headers:  map[string]string{},
			}}
		}
		if len(p.Credentials) == 0 && strings.TrimSpace(p.APIKey) != "" {
			p.Credentials = []ProviderCredential{{
				ID:      NewID("cred"),
				Label:   "default",
				APIKey:  strings.TrimSpace(p.APIKey),
				Enabled: true,
				Weight:  1,
				Headers: map[string]string{},
			}}
		}
		for endpointIndex := range p.Endpoints {
			endpoint := &p.Endpoints[endpointIndex]
			if endpoint.ID == "" {
				endpoint.ID = NewID("endpoint")
			}
			if endpoint.Weight <= 0 {
				endpoint.Weight = 1
			}
			if endpoint.Headers == nil {
				endpoint.Headers = map[string]string{}
			}
			endpoint.BaseURL = strings.TrimSpace(endpoint.BaseURL)
		}
		for credentialIndex := range p.Credentials {
			credential := &p.Credentials[credentialIndex]
			if credential.ID == "" {
				credential.ID = NewID("cred")
			}
			if credential.Weight <= 0 {
				credential.Weight = 1
			}
			if credential.Headers == nil {
				credential.Headers = map[string]string{}
			}
			credential.APIKey = strings.TrimSpace(credential.APIKey)
		}

		p.BaseURL = firstEnabledEndpointURL(*p)
		p.APIKey = firstEnabledCredentialKey(*p)
	}

	for i := range s.ModelRoutes {
		r := &s.ModelRoutes[i]
		for j := range r.Targets {
			t := &r.Targets[j]
			if t.ID == "" {
				t.ID = NewID("target")
			}
			if t.Weight <= 0 {
				t.Weight = 1
			}
			if t.MarkupMultiplier <= 0 {
				t.MarkupMultiplier = 1
			}
			if t.Protocols == nil {
				t.Protocols = []Protocol{}
			}
		}
	}

	for i := range s.Integrations {
		integration := &s.Integrations[i]
		if integration.ID == "" {
			integration.ID = NewID("integration")
		}
		if integration.DefaultMarkupMultiplier <= 0 {
			integration.DefaultMarkupMultiplier = 1
		}
		if integration.ModelMarkupOverrides == nil {
			integration.ModelMarkupOverrides = map[string]float64{}
		}
		if integration.Snapshot.Prices == nil {
			integration.Snapshot.Prices = map[string]IntegrationPricing{}
		}
		if integration.DefaultProtocols == nil || len(integration.DefaultProtocols) == 0 {
			integration.DefaultProtocols = []Protocol{
				ProtocolOpenAIChat,
				ProtocolOpenAIResponses,
				ProtocolAnthropic,
				ProtocolGeminiGenerate,
				ProtocolGeminiStream,
			}
		}
	}

	for i := range s.GatewayKeys {
		key := &s.GatewayKeys[i]
		if key.ID == "" {
			key.ID = NewID("gk")
		}
		if key.Name == "" {
			key.Name = key.ID
		}
		if key.AllowedModels == nil {
			key.AllowedModels = []string{}
		}
		if key.AllowedProtocols == nil {
			key.AllowedProtocols = []Protocol{}
		}
	}
}

func firstEnabledEndpointURL(provider Provider) string {
	for _, endpoint := range provider.Endpoints {
		if endpoint.Enabled && strings.TrimSpace(endpoint.BaseURL) != "" {
			return strings.TrimSpace(endpoint.BaseURL)
		}
	}
	return strings.TrimSpace(provider.BaseURL)
}

func firstEnabledCredentialKey(provider Provider) string {
	for _, credential := range provider.Credentials {
		if credential.Enabled && strings.TrimSpace(credential.APIKey) != "" {
			return strings.TrimSpace(credential.APIKey)
		}
	}
	return strings.TrimSpace(provider.APIKey)
}

// Supports reports whether the provider can be used for the protocol on the
// public gateway surface.
func (p Provider) Supports(protocol Protocol) bool {
	return slices.Contains(p.Capabilities, protocol)
}

// Supports reports whether the route target allows the protocol.
func (t RouteTarget) Supports(protocol Protocol) bool {
	if len(t.Protocols) == 0 {
		return true
	}
	return slices.Contains(t.Protocols, protocol)
}

// AllowsProtocol reports whether the gateway key can be used for a protocol.
func (k GatewayKey) AllowsProtocol(protocol Protocol) bool {
	return len(k.AllowedProtocols) == 0 || slices.Contains(k.AllowedProtocols, protocol)
}

// AllowsModel reports whether the gateway key can be used for a route alias.
func (k GatewayKey) AllowsModel(alias string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, item := range k.AllowedModels {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(alias)) {
			return true
		}
	}
	return false
}

// IsExpired reports whether the gateway key is currently expired.
func (k GatewayKey) IsExpired(now time.Time) bool {
	return k.ExpiresAt != nil && now.After(*k.ExpiresAt)
}

// FindProvider returns one provider by ID.
func (s State) FindProvider(id string) (Provider, bool) {
	for _, provider := range s.Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return Provider{}, false
}

// FindRoute returns one route by alias, case-insensitively.
func (s State) FindRoute(alias string) (ModelRoute, bool) {
	for _, route := range s.ModelRoutes {
		if strings.EqualFold(route.Alias, alias) {
			return route, true
		}
	}
	return ModelRoute{}, false
}

// FindPricingProfile returns one pricing profile by ID.
func (s State) FindPricingProfile(id string) (PricingProfile, bool) {
	for _, profile := range s.PricingProfiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return PricingProfile{}, false
}

// FindIntegration returns one integration by ID.
func (s State) FindIntegration(id string) (Integration, bool) {
	for _, integration := range s.Integrations {
		if integration.ID == id {
			return integration, true
		}
	}
	return Integration{}, false
}

// FindGatewayKey returns one consumer key by ID.
func (s State) FindGatewayKey(id string) (GatewayKey, bool) {
	for _, key := range s.GatewayKeys {
		if key.ID == id {
			return key, true
		}
	}
	return GatewayKey{}, false
}

// RoutingSnapshot clones the hot-path state.
func (s State) RoutingSnapshot() RoutingState {
	snapshot := RoutingState{
		Providers:       make([]Provider, len(s.Providers)),
		ModelRoutes:     make([]ModelRoute, len(s.ModelRoutes)),
		PricingProfiles: slices.Clone(s.PricingProfiles),
		GatewayKeys:     make([]GatewayKey, len(s.GatewayKeys)),
	}
	for i, provider := range s.Providers {
		snapshot.Providers[i] = provider.clone()
	}
	for i, route := range s.ModelRoutes {
		snapshot.ModelRoutes[i] = route.clone()
	}
	for i, key := range s.GatewayKeys {
		snapshot.GatewayKeys[i] = key.clone()
	}
	return snapshot
}

// FindProvider returns one provider by ID.
func (s RoutingState) FindProvider(id string) (Provider, bool) {
	for _, provider := range s.Providers {
		if provider.ID == id {
			return provider, true
		}
	}
	return Provider{}, false
}

// FindRoute returns one route by alias.
func (s RoutingState) FindRoute(alias string) (ModelRoute, bool) {
	for _, route := range s.ModelRoutes {
		if strings.EqualFold(route.Alias, alias) {
			return route, true
		}
	}
	return ModelRoute{}, false
}

// FindPricingProfile returns one pricing profile by ID.
func (s RoutingState) FindPricingProfile(id string) (PricingProfile, bool) {
	for _, profile := range s.PricingProfiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return PricingProfile{}, false
}

// FindGatewayKeyByHash returns one gateway key by its secret hash.
func (s RoutingState) FindGatewayKeyByHash(secretHash string) (GatewayKey, bool) {
	for _, key := range s.GatewayKeys {
		if key.SecretHash == secretHash {
			return key, true
		}
	}
	return GatewayKey{}, false
}

// Clone returns a deep copy.
func (s State) Clone() State {
	cloned := s
	routing := s.RoutingSnapshot()
	cloned.Providers = routing.Providers
	cloned.ModelRoutes = routing.ModelRoutes
	cloned.PricingProfiles = routing.PricingProfiles
	cloned.GatewayKeys = make([]GatewayKey, len(s.GatewayKeys))
	for i, key := range s.GatewayKeys {
		cloned.GatewayKeys[i] = key.clone()
	}
	cloned.Integrations = make([]Integration, len(s.Integrations))
	for i, integration := range s.Integrations {
		cloned.Integrations[i] = integration.clone()
	}
	cloned.RequestHistory = slices.Clone(s.RequestHistory)
	cloned.Normalize()
	return cloned
}

// TrimHistory keeps only the newest maxItems entries.
func TrimHistory(records []RequestRecord, maxItems int) []RequestRecord {
	if maxItems <= 0 || len(records) <= maxItems {
		return records
	}
	return append([]RequestRecord(nil), records[len(records)-maxItems:]...)
}

func (p Provider) clone() Provider {
	p.Capabilities = slices.Clone(p.Capabilities)
	p.Headers = maps.Clone(p.Headers)
	p.CircuitBreaker.Enabled = cloneBoolPointer(p.CircuitBreaker.Enabled)
	endpoints := make([]ProviderEndpoint, len(p.Endpoints))
	for i, endpoint := range p.Endpoints {
		endpoint.Headers = maps.Clone(endpoint.Headers)
		endpoints[i] = endpoint
	}
	p.Endpoints = endpoints
	credentials := make([]ProviderCredential, len(p.Credentials))
	for i, credential := range p.Credentials {
		credential.Headers = maps.Clone(credential.Headers)
		credentials[i] = credential
	}
	p.Credentials = credentials
	return p
}

func (r ModelRoute) clone() ModelRoute {
	targets := make([]RouteTarget, len(r.Targets))
	for i, target := range r.Targets {
		targets[i] = target.clone()
	}
	r.Targets = targets
	return r
}

func (t RouteTarget) clone() RouteTarget {
	t.Protocols = slices.Clone(t.Protocols)
	return t
}

func (k GatewayKey) clone() GatewayKey {
	k.ExpiresAt = cloneTimePointer(k.ExpiresAt)
	k.LastUsedAt = cloneTimePointer(k.LastUsedAt)
	k.AllowedModels = slices.Clone(k.AllowedModels)
	k.AllowedProtocols = slices.Clone(k.AllowedProtocols)
	return k
}

func (i Integration) clone() Integration {
	i.DefaultProtocols = slices.Clone(i.DefaultProtocols)
	i.ModelMarkupOverrides = maps.Clone(i.ModelMarkupOverrides)
	i.Snapshot = i.Snapshot.clone()
	return i
}

func (s IntegrationSnapshot) clone() IntegrationSnapshot {
	s.ModelNames = slices.Clone(s.ModelNames)
	s.Prices = maps.Clone(s.Prices)
	return s
}

// IsEnabled reports whether the circuit breaker should actively trip.
func (c CircuitBreakerConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func boolPointer(value bool) *bool {
	v := value
	return &v
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	return boolPointer(*value)
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	next := value.UTC()
	return &next
}
