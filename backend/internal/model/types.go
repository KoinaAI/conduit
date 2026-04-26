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

func defaultProviderCapabilities(kind ProviderKind) []Protocol {
	switch kind {
	case ProviderKindAnthropic:
		return []Protocol{ProtocolAnthropic, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	case ProviderKindGemini:
		return []Protocol{ProtocolGeminiGenerate, ProtocolGeminiStream, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	default:
		return []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses}
	}
}

func providerKindSupportsProtocol(kind ProviderKind, protocol Protocol) bool {
	switch kind {
	case ProviderKindAnthropic:
		return protocol == ProtocolAnthropic || protocol == ProtocolOpenAIChat || protocol == ProtocolOpenAIResponses
	case ProviderKindGemini:
		return protocol == ProtocolGeminiGenerate || protocol == ProtocolGeminiStream || protocol == ProtocolOpenAIChat || protocol == ProtocolOpenAIResponses
	default:
		switch protocol {
		case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolOpenAIRealtime, ProtocolAnthropic, ProtocolGeminiGenerate, ProtocolGeminiStream:
			return true
		default:
			return false
		}
	}
}

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
	ProviderRoutingModeWeighted   ProviderRoutingMode = "weighted"
	ProviderRoutingModeLatency    ProviderRoutingMode = "latency"
	ProviderRoutingModeRoundRobin ProviderRoutingMode = "round-robin"
	ProviderRoutingModeRandom     ProviderRoutingMode = "random"
	ProviderRoutingModeFailover   ProviderRoutingMode = "failover"
	ProviderRoutingModeOrdered    ProviderRoutingMode = "ordered"
)

// RouteStrategy controls how candidates are ordered for one public model route.
type RouteStrategy string

const (
	RouteStrategyPriorityWeight RouteStrategy = "priority-weight"
	RouteStrategyLatency        RouteStrategy = "latency"
	RouteStrategyRoundRobin     RouteStrategy = "round-robin"
	RouteStrategyRandom         RouteStrategy = "random"
	RouteStrategyFailover       RouteStrategy = "failover"
)

type TransformerPhase string

const (
	TransformerPhaseRequest  TransformerPhase = "request"
	TransformerPhaseResponse TransformerPhase = "response"
)

type TransformerType string

const (
	TransformerTypeSetHeader      TransformerType = "set_header"
	TransformerTypeRemoveHeader   TransformerType = "remove_header"
	TransformerTypeSetJSON        TransformerType = "set_json"
	TransformerTypeDeleteJSON     TransformerType = "delete_json"
	TransformerTypePrependMessage TransformerType = "prepend_message"
)

type PricingAliasMatchType string

const (
	PricingAliasMatchExact    PricingAliasMatchType = "exact"
	PricingAliasMatchPrefix   PricingAliasMatchType = "prefix"
	PricingAliasMatchWildcard PricingAliasMatchType = "wildcard"
)

// State is the persisted configuration snapshot exposed to the admin API.
type State struct {
	Version         string             `json:"version"`
	Providers       []Provider         `json:"providers"`
	ModelRoutes     []ModelRoute       `json:"model_routes"`
	PricingProfiles []PricingProfile   `json:"pricing_profiles"`
	PricingAliases  []PricingAliasRule `json:"pricing_aliases"`
	Integrations    []Integration      `json:"integrations"`
	GatewayKeys     []GatewayKey       `json:"gateway_keys"`
	RequestHistory  []RequestRecord    `json:"request_history"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// RoutingState is the read-only subset needed on the hot path.
type RoutingState struct {
	Providers       []Provider
	ModelRoutes     []ModelRoute
	PricingProfiles []PricingProfile
	PricingAliases  []PricingAliasRule
	GatewayKeys     []GatewayKey
}

// Provider defines one upstream provider inventory item.
type Provider struct {
	ID                      string               `json:"id"`
	Name                    string               `json:"name"`
	Kind                    ProviderKind         `json:"kind"`
	ProxyURL                string               `json:"proxy_url,omitempty"`
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
	MaxConcurrency          int                  `json:"max_concurrency,omitempty"`
	RateLimitRPM            int                  `json:"rate_limit_rpm,omitempty"`
	HourlyBudgetUSD         float64              `json:"hourly_budget_usd,omitempty"`
	DailyBudgetUSD          float64              `json:"daily_budget_usd,omitempty"`
	WeeklyBudgetUSD         float64              `json:"weekly_budget_usd,omitempty"`
	MonthlyBudgetUSD        float64              `json:"monthly_budget_usd,omitempty"`
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
	Alias            string             `json:"alias"`
	Strategy         RouteStrategy      `json:"strategy,omitempty"`
	PricingProfileID string             `json:"pricing_profile_id,omitempty"`
	Targets          []RouteTarget      `json:"targets"`
	Scenarios        []RouteScenario    `json:"scenarios,omitempty"`
	Transformers     []RouteTransformer `json:"transformers,omitempty"`
	Notes            string             `json:"notes,omitempty"`
}

// RouteTransformer defines one ordered request/response mutation step.
type RouteTransformer struct {
	Name   string           `json:"name,omitempty"`
	Phase  TransformerPhase `json:"phase"`
	Type   TransformerType  `json:"type"`
	Target string           `json:"target,omitempty"`
	Value  any              `json:"value,omitempty"`
}

// RouteScenario overrides route targets and strategy for one named scenario.
type RouteScenario struct {
	Name     string        `json:"name"`
	Strategy RouteStrategy `json:"strategy,omitempty"`
	Targets  []RouteTarget `json:"targets"`
	Notes    string        `json:"notes,omitempty"`
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

// PricingAliasRule maps one route alias or upstream model pattern to an
// existing pricing profile.
type PricingAliasRule struct {
	Name             string                `json:"name,omitempty"`
	MatchType        PricingAliasMatchType `json:"match_type"`
	Pattern          string                `json:"pattern"`
	PricingProfileID string                `json:"pricing_profile_id"`
	Enabled          bool                  `json:"enabled"`
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
	AutoSyncPricingProfiles bool                `json:"auto_sync_pricing_profiles,omitempty"`
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
	HourlyBudgetUSD  float64    `json:"hourly_budget_usd,omitempty"`
	DailyBudgetUSD   float64    `json:"daily_budget_usd,omitempty"`
	WeeklyBudgetUSD  float64    `json:"weekly_budget_usd,omitempty"`
	MonthlyBudgetUSD float64    `json:"monthly_budget_usd,omitempty"`
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
	ID              string           `json:"id"`
	Protocol        Protocol         `json:"protocol"`
	RouteAlias      string           `json:"route_alias"`
	AccountID       string           `json:"account_id"`
	ProviderName    string           `json:"provider_name"`
	UpstreamModel   string           `json:"upstream_model"`
	GatewayKeyID    string           `json:"gateway_key_id,omitempty"`
	ClientSessionID string           `json:"client_session_id,omitempty"`
	Path            string           `json:"path,omitempty"`
	StatusCode      int              `json:"status_code"`
	Stream          bool             `json:"stream"`
	AttemptCount    int              `json:"attempt_count"`
	StartedAt       time.Time        `json:"started_at"`
	DurationMS      int64            `json:"duration_ms"`
	Usage           UsageSummary     `json:"usage"`
	Billing         BillingSummary   `json:"billing"`
	RoutingDecision *RoutingDecision `json:"routing_decision,omitempty"`
	Error           string           `json:"error,omitempty"`
}

// RoutingDecision captures how the gateway selected and retried upstream candidates.
type RoutingDecision struct {
	Strategy   RouteStrategy          `json:"strategy"`
	Scenario   string                 `json:"scenario,omitempty"`
	SessionID  string                 `json:"session_id,omitempty"`
	Candidates []RoutingCandidate     `json:"candidates,omitempty"`
	Selected   *RoutingCandidate      `json:"selected,omitempty"`
	Events     []RoutingDecisionEvent `json:"events,omitempty"`
}

// RoutingCandidate summarizes one candidate considered for a request.
type RoutingCandidate struct {
	ProviderID    string `json:"provider_id"`
	ProviderName  string `json:"provider_name"`
	EndpointID    string `json:"endpoint_id"`
	CredentialID  string `json:"credential_id"`
	UpstreamModel string `json:"upstream_model"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	Healthy       bool   `json:"healthy"`
	Sticky        bool   `json:"sticky,omitempty"`
}

// RoutingDecisionEvent records one retry/failover outcome within a request.
type RoutingDecisionEvent struct {
	Attempt      int    `json:"attempt"`
	ProviderID   string `json:"provider_id,omitempty"`
	EndpointID   string `json:"endpoint_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	Decision     string `json:"decision"`
	StatusCode   int    `json:"status_code,omitempty"`
	Retryable    bool   `json:"retryable,omitempty"`
	BackoffMS    int64  `json:"backoff_ms,omitempty"`
	Error        string `json:"error,omitempty"`
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
		PricingAliases:  []PricingAliasRule{},
		Integrations:    []Integration{},
		GatewayKeys:     []GatewayKey{},
		RequestHistory:  []RequestRecord{},
		UpdatedAt:       time.Now().UTC(),
	}
}

// Normalize fills defaults.
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
	if s.PricingAliases == nil {
		s.PricingAliases = []PricingAliasRule{}
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
		p.ProxyURL = strings.TrimSpace(p.ProxyURL)
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
		if p.MaxConcurrency < 0 {
			p.MaxConcurrency = 0
		}
		if p.RateLimitRPM < 0 {
			p.RateLimitRPM = 0
		}
		if p.HourlyBudgetUSD < 0 {
			p.HourlyBudgetUSD = 0
		}
		if p.DailyBudgetUSD < 0 {
			p.DailyBudgetUSD = 0
		}
		if p.WeeklyBudgetUSD < 0 {
			p.WeeklyBudgetUSD = 0
		}
		if p.MonthlyBudgetUSD < 0 {
			p.MonthlyBudgetUSD = 0
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
		if len(p.Capabilities) == 0 {
			p.Capabilities = defaultProviderCapabilities(p.Kind)
		} else {
			filtered := make([]Protocol, 0, len(p.Capabilities))
			for _, capability := range p.Capabilities {
				if providerKindSupportsProtocol(p.Kind, capability) && !slices.Contains(filtered, capability) {
					filtered = append(filtered, capability)
				}
			}
			if len(filtered) == 0 {
				filtered = defaultProviderCapabilities(p.Kind)
			}
			p.Capabilities = filtered
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
	}
	for i := range s.PricingAliases {
		rule := &s.PricingAliases[i]
		rule.Name = strings.TrimSpace(rule.Name)
		rule.Pattern = strings.TrimSpace(rule.Pattern)
		rule.PricingProfileID = strings.TrimSpace(rule.PricingProfileID)
		rule.MatchType = rule.MatchType.Canonical()
	}

	for i := range s.ModelRoutes {
		r := &s.ModelRoutes[i]
		if canonical := r.Strategy.Canonical(); canonical != "" {
			r.Strategy = canonical
		} else {
			r.Strategy = RouteStrategyPriorityWeight
		}
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
		if r.Scenarios == nil {
			r.Scenarios = []RouteScenario{}
		}
		if r.Transformers == nil {
			r.Transformers = []RouteTransformer{}
		}
		for j := range r.Transformers {
			transformer := &r.Transformers[j]
			transformer.Name = strings.TrimSpace(transformer.Name)
			transformer.Target = strings.TrimSpace(transformer.Target)
			transformer.Phase = transformer.Phase.Canonical()
			transformer.Type = transformer.Type.Canonical()
			transformer.Value = cloneJSONValue(transformer.Value)
		}
		for j := range r.Scenarios {
			scenario := &r.Scenarios[j]
			scenario.Name = strings.TrimSpace(scenario.Name)
			if canonical := scenario.Strategy.Canonical(); canonical != "" {
				scenario.Strategy = canonical
			}
			if scenario.Targets == nil {
				scenario.Targets = []RouteTarget{}
			}
			for k := range scenario.Targets {
				target := &scenario.Targets[k]
				if target.ID == "" {
					target.ID = NewID("target")
				}
				if target.Weight <= 0 {
					target.Weight = 1
				}
				if target.MarkupMultiplier <= 0 {
					target.MarkupMultiplier = 1
				}
				if target.Protocols == nil {
					target.Protocols = []Protocol{}
				}
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
		if len(integration.DefaultProtocols) == 0 {
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

// Canonical returns the normalized strategy value or the empty string when unsupported.
func (s RouteStrategy) Canonical() RouteStrategy {
	switch strings.ToLower(strings.TrimSpace(string(s))) {
	case string(RouteStrategyPriorityWeight):
		return RouteStrategyPriorityWeight
	case "":
		return ""
	case string(RouteStrategyLatency):
		return RouteStrategyLatency
	case string(RouteStrategyRoundRobin):
		return RouteStrategyRoundRobin
	case string(RouteStrategyRandom):
		return RouteStrategyRandom
	case string(RouteStrategyFailover):
		return RouteStrategyFailover
	default:
		return ""
	}
}

func (p TransformerPhase) Canonical() TransformerPhase {
	switch strings.ToLower(strings.TrimSpace(string(p))) {
	case string(TransformerPhaseRequest):
		return TransformerPhaseRequest
	case string(TransformerPhaseResponse):
		return TransformerPhaseResponse
	default:
		return ""
	}
}

func (t TransformerType) Canonical() TransformerType {
	switch strings.ToLower(strings.TrimSpace(string(t))) {
	case string(TransformerTypeSetHeader):
		return TransformerTypeSetHeader
	case string(TransformerTypeRemoveHeader):
		return TransformerTypeRemoveHeader
	case string(TransformerTypeSetJSON):
		return TransformerTypeSetJSON
	case string(TransformerTypeDeleteJSON):
		return TransformerTypeDeleteJSON
	case string(TransformerTypePrependMessage):
		return TransformerTypePrependMessage
	default:
		return ""
	}
}

func (t PricingAliasMatchType) Canonical() PricingAliasMatchType {
	switch strings.ToLower(strings.TrimSpace(string(t))) {
	case string(PricingAliasMatchExact):
		return PricingAliasMatchExact
	case string(PricingAliasMatchPrefix):
		return PricingAliasMatchPrefix
	case string(PricingAliasMatchWildcard):
		return PricingAliasMatchWildcard
	default:
		return ""
	}
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

// ResolvePricingProfile chooses the effective pricing profile for one request.
func (s State) ResolvePricingProfile(explicitID, routeAlias, upstreamModel string) (PricingProfile, bool) {
	if explicitID = strings.TrimSpace(explicitID); explicitID != "" {
		if profile, ok := s.FindPricingProfile(explicitID); ok {
			return profile, true
		}
	}
	return resolvePricingProfileByAlias(s.PricingProfiles, s.PricingAliases, routeAlias, upstreamModel)
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
		PricingAliases:  slices.Clone(s.PricingAliases),
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

// ResolvePricingProfile chooses the effective pricing profile for one request.
func (s RoutingState) ResolvePricingProfile(explicitID, routeAlias, upstreamModel string) (PricingProfile, bool) {
	if explicitID = strings.TrimSpace(explicitID); explicitID != "" {
		if profile, ok := s.FindPricingProfile(explicitID); ok {
			return profile, true
		}
	}
	return resolvePricingProfileByAlias(s.PricingProfiles, s.PricingAliases, routeAlias, upstreamModel)
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
	cloned.PricingAliases = slices.Clone(s.PricingAliases)
	cloned.GatewayKeys = make([]GatewayKey, len(s.GatewayKeys))
	for i, key := range s.GatewayKeys {
		cloned.GatewayKeys[i] = key.clone()
	}
	cloned.Integrations = make([]Integration, len(s.Integrations))
	for i, integration := range s.Integrations {
		cloned.Integrations[i] = integration.clone()
	}
	cloned.RequestHistory = make([]RequestRecord, len(s.RequestHistory))
	for i, record := range s.RequestHistory {
		cloned.RequestHistory[i] = record.clone()
	}
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
	scenarios := make([]RouteScenario, len(r.Scenarios))
	for i, scenario := range r.Scenarios {
		scenarios[i] = scenario.clone()
	}
	r.Scenarios = scenarios
	transformers := make([]RouteTransformer, len(r.Transformers))
	for i, transformer := range r.Transformers {
		transformers[i] = transformer.clone()
	}
	r.Transformers = transformers
	return r
}

func (s RouteScenario) clone() RouteScenario {
	targets := make([]RouteTarget, len(s.Targets))
	for i, target := range s.Targets {
		targets[i] = target.clone()
	}
	s.Targets = targets
	return s
}

func (t RouteTarget) clone() RouteTarget {
	t.Protocols = slices.Clone(t.Protocols)
	return t
}

func (t RouteTransformer) clone() RouteTransformer {
	t.Value = cloneJSONValue(t.Value)
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

func (r RequestRecord) clone() RequestRecord {
	if r.RoutingDecision != nil {
		decision := r.RoutingDecision.clone()
		r.RoutingDecision = &decision
	}
	return r
}

func (d RoutingDecision) clone() RoutingDecision {
	d.Candidates = slices.Clone(d.Candidates)
	d.Events = slices.Clone(d.Events)
	if d.Selected != nil {
		selected := *d.Selected
		d.Selected = &selected
	}
	return d
}

func cloneJSONValue(value any) any {
	switch current := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(current))
		for key, item := range current {
			cloned[key] = cloneJSONValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(current))
		for index, item := range current {
			cloned[index] = cloneJSONValue(item)
		}
		return cloned
	default:
		return current
	}
}

func (s IntegrationSnapshot) clone() IntegrationSnapshot {
	s.ModelNames = slices.Clone(s.ModelNames)
	s.Prices = maps.Clone(s.Prices)
	return s
}

func resolvePricingProfileByAlias(profiles []PricingProfile, rules []PricingAliasRule, routeAlias, upstreamModel string) (PricingProfile, bool) {
	candidates := []string{strings.TrimSpace(routeAlias), strings.TrimSpace(upstreamModel)}
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if rule.MatchType.Canonical() == "" || strings.TrimSpace(rule.Pattern) == "" || strings.TrimSpace(rule.PricingProfileID) == "" {
			continue
		}
		for _, candidate := range candidates {
			if candidate == "" || !pricingAliasMatches(rule, candidate) {
				continue
			}
			for _, profile := range profiles {
				if profile.ID == rule.PricingProfileID {
					return profile, true
				}
			}
		}
	}
	return PricingProfile{}, false
}

func pricingAliasMatches(rule PricingAliasRule, value string) bool {
	pattern := strings.TrimSpace(rule.Pattern)
	value = strings.TrimSpace(value)
	if pattern == "" || value == "" {
		return false
	}
	switch rule.MatchType.Canonical() {
	case PricingAliasMatchExact:
		return strings.EqualFold(pattern, value)
	case PricingAliasMatchPrefix:
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(pattern))
	case PricingAliasMatchWildcard:
		return wildcardMatch(pattern, value)
	default:
		return false
	}
}

func wildcardMatch(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	if len(parts) > 2 {
		return false
	}
	left := parts[0]
	right := parts[1]
	if left != "" && !strings.HasPrefix(value, left) {
		return false
	}
	if right != "" && !strings.HasSuffix(value, right) {
		return false
	}
	return len(value) >= len(left)+len(right)
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
