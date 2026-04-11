package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type Service struct {
	client               *http.Client
	transport            *http.Transport
	allowPrivateBaseURLs bool
	lookupIPs            func(context.Context, string, string) ([]net.IP, error)
	pinnedClientsMu      sync.Mutex
	pinnedClients        map[string]pinnedClient
}

type resolvedBaseURL struct {
	BaseURL     string
	ClientKey   string
	DialAddress string
}

type pinnedClient struct {
	DialAddress string
	Client      *http.Client
	Transport   *http.Transport
}

type Option func(*Service)

type SyncResult struct {
	Snapshot         model.IntegrationSnapshot
	ModelsRefreshed  bool
	PricingRefreshed bool
	FinishedAt       time.Time
	Err              error
}

type CheckinResult struct {
	Message    string
	At         *time.Time
	FinishedAt time.Time
	Err        error
}

type DailyCheckinResult struct {
	IntegrationID string
	Result        CheckinResult
}

const maxIntegrationResponseBodyBytes int64 = 4 << 20

var errIntegrationResponseTooLarge = errors.New("integration response too large")

var integrationTracer = otel.Tracer("github.com/KoinaAI/conduit/backend/internal/integration")

func NewService(options ...Option) *Service {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	service := &Service{
		client:        &http.Client{Timeout: 20 * time.Second, Transport: transport},
		transport:     transport,
		lookupIPs:     net.DefaultResolver.LookupIP,
		pinnedClients: map[string]pinnedClient{},
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	s.pinnedClientsMu.Lock()
	for key, client := range s.pinnedClients {
		if client.Transport != nil {
			client.Transport.CloseIdleConnections()
		}
		delete(s.pinnedClients, key)
	}
	s.pinnedClientsMu.Unlock()
	return nil
}

// WithAllowPrivateBaseURLForTests relaxes SSRF protection for local tests only.
func WithAllowPrivateBaseURLForTests() Option {
	return func(service *Service) {
		service.allowPrivateBaseURLs = true
	}
}

func (s *Service) SyncState(ctx context.Context, state *model.State, integrationID string) (model.IntegrationSnapshot, error) {
	integration, ok := state.FindIntegration(integrationID)
	if !ok {
		return model.IntegrationSnapshot{}, fmt.Errorf("integration %q not found", integrationID)
	}

	return s.ApplySyncResult(state, integrationID, s.PrepareSync(ctx, integration))
}

func (s *Service) CheckinState(ctx context.Context, state *model.State, integrationID string) error {
	integration, ok := state.FindIntegration(integrationID)
	if !ok {
		return fmt.Errorf("integration %q not found", integrationID)
	}

	return s.ApplyCheckinResult(state, integrationID, s.PrepareCheckin(ctx, integration))
}

// ValidateBaseURL rejects unsafe integration management endpoints.
func (s *Service) ValidateBaseURL(baseURL string) error {
	_, err := s.resolveBaseURL(context.Background(), baseURL)
	return err
}

func (s *Service) PrepareSync(ctx context.Context, integration model.Integration) SyncResult {
	ctx, span := integrationTracer.Start(ctx, "integration.prepare_sync",
		trace.WithAttributes(
			attribute.String("integration.id", integration.ID),
			attribute.String("integration.kind", string(integration.Kind)),
			attribute.String("integration.base_url", strings.TrimSpace(integration.BaseURL)),
		),
	)
	defer span.End()
	outcome, err := s.sync(ctx, integration)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetAttributes(attribute.Int("integration.model_count", len(outcome.Snapshot.ModelNames)))
	}
	return SyncResult{
		Snapshot:         outcome.Snapshot,
		ModelsRefreshed:  outcome.ModelsRefreshed,
		PricingRefreshed: outcome.PricingRefreshed,
		FinishedAt:       time.Now().UTC(),
		Err:              err,
	}
}

func (s *Service) ApplySyncResult(state *model.State, integrationID string, result SyncResult) (model.IntegrationSnapshot, error) {
	index := findIntegrationIndex(state.Integrations, integrationID)
	if index < 0 {
		return model.IntegrationSnapshot{}, fmt.Errorf("integration %q not found", integrationID)
	}

	integration := state.Integrations[index]
	integration.Snapshot.LastSyncAt = &result.FinishedAt
	if result.Err != nil {
		integration.Snapshot.LastError = result.Err.Error()
		state.Integrations[index] = integration
		return model.IntegrationSnapshot{}, result.Err
	}

	lastCheckinAt := integration.Snapshot.LastCheckinAt
	lastCheckinResult := integration.Snapshot.LastCheckinResult
	nextSnapshot := integration.Snapshot
	nextSnapshot.Balance = result.Snapshot.Balance
	nextSnapshot.Used = result.Snapshot.Used
	nextSnapshot.Currency = result.Snapshot.Currency
	nextSnapshot.SupportsCheckin = result.Snapshot.SupportsCheckin
	nextSnapshot.LastError = result.Snapshot.LastError
	if result.ModelsRefreshed {
		nextSnapshot.ModelNames = slices.Clone(result.Snapshot.ModelNames)
	}
	if result.PricingRefreshed {
		nextSnapshot.Prices = mapsClonePricing(result.Snapshot.Prices)
	}
	integration.Snapshot = nextSnapshot
	integration.Snapshot.LastCheckinAt = lastCheckinAt
	integration.Snapshot.LastCheckinResult = lastCheckinResult
	integration.Snapshot.LastSyncAt = &result.FinishedAt
	integration.UpdatedAt = result.FinishedAt

	provider := buildProviderFromIntegration(integration)
	integration.LinkedProviderID = provider.ID
	upsertProvider(&state.Providers, provider)
	if integration.AutoCreateRoutes && result.ModelsRefreshed {
		syncAutoRoutes(state, integration, result.Snapshot)
	}
	if integration.AutoSyncPricingProfiles && result.PricingRefreshed {
		syncManagedPricingProfiles(state, integration, result.Snapshot)
	}

	state.Integrations[index] = integration
	return result.Snapshot, nil
}

func (s *Service) PrepareCheckin(ctx context.Context, integration model.Integration) CheckinResult {
	ctx, span := integrationTracer.Start(ctx, "integration.prepare_checkin",
		trace.WithAttributes(
			attribute.String("integration.id", integration.ID),
			attribute.String("integration.kind", string(integration.Kind)),
			attribute.String("integration.base_url", strings.TrimSpace(integration.BaseURL)),
		),
	)
	defer span.End()
	message, at, err := s.checkin(ctx, integration)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return CheckinResult{
		Message:    message,
		At:         at,
		FinishedAt: time.Now().UTC(),
		Err:        err,
	}
}

func (s *Service) ApplyCheckinResult(state *model.State, integrationID string, result CheckinResult) error {
	index := findIntegrationIndex(state.Integrations, integrationID)
	if index < 0 {
		return fmt.Errorf("integration %q not found", integrationID)
	}

	integration := state.Integrations[index]
	integration.Snapshot.LastCheckinResult = result.Message
	integration.Snapshot.LastCheckinAt = result.At
	integration.UpdatedAt = result.FinishedAt
	if result.Err != nil {
		integration.Snapshot.LastError = result.Err.Error()
		state.Integrations[index] = integration
		return result.Err
	}
	integration.Snapshot.LastError = ""
	state.Integrations[index] = integration
	return nil
}

func (s *Service) PrepareDailyCheckins(ctx context.Context, state model.State, now time.Time) []DailyCheckinResult {
	results := make([]DailyCheckinResult, 0, len(state.Integrations))
	for _, integration := range state.Integrations {
		if !NeedsCheckin(integration, now) {
			continue
		}
		results = append(results, DailyCheckinResult{
			IntegrationID: integration.ID,
			Result:        s.PrepareCheckin(ctx, integration),
		})
	}
	return results
}

func (s *Service) ApplyDailyCheckins(state *model.State, results []DailyCheckinResult) []error {
	errs := make([]error, 0, len(results))
	for _, result := range results {
		if err := s.ApplyCheckinResult(state, result.IntegrationID, result.Result); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

type syncOutcome struct {
	Snapshot         model.IntegrationSnapshot
	ModelsRefreshed  bool
	PricingRefreshed bool
}

func (s *Service) sync(ctx context.Context, integration model.Integration) (syncOutcome, error) {
	switch integration.Kind {
	case model.IntegrationKindNewAPI:
		return s.syncNewAPI(ctx, integration)
	case model.IntegrationKindOneHub:
		return s.syncOneHub(ctx, integration)
	default:
		return syncOutcome{}, fmt.Errorf("unsupported integration kind: %s", integration.Kind)
	}
}

func (s *Service) checkin(ctx context.Context, integration model.Integration) (string, *time.Time, error) {
	switch integration.Kind {
	case model.IntegrationKindNewAPI:
		return s.checkinNewAPI(ctx, integration)
	case model.IntegrationKindOneHub:
		return s.checkinOneHub(ctx, integration)
	default:
		return "unsupported", nil, fmt.Errorf("unsupported integration kind: %s", integration.Kind)
	}
}

func buildProviderFromIntegration(integration model.Integration) model.Provider {
	now := time.Now().UTC()
	providerID := integration.LinkedProviderID
	if providerID == "" {
		providerID = model.NewID("provider")
	}

	capabilities := integration.DefaultProtocols
	if len(capabilities) == 0 {
		capabilities = []model.Protocol{
			model.ProtocolOpenAIChat,
			model.ProtocolOpenAIResponses,
			model.ProtocolAnthropic,
			model.ProtocolGeminiGenerate,
			model.ProtocolGeminiStream,
		}
	}

	apiKey := strings.TrimSpace(integration.RelayAPIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(integration.AccessKey)
	}

	return model.Provider{
		ID:                      providerID,
		Name:                    integration.Name,
		Kind:                    model.ProviderKindOpenAICompatible,
		Enabled:                 integration.Enabled,
		Weight:                  1,
		TimeoutSeconds:          180,
		DefaultMarkupMultiplier: 1,
		Capabilities:            capabilities,
		Headers:                 map[string]string{},
		RoutingMode:             model.ProviderRoutingModeLatency,
		MaxAttempts:             3,
		StickySessionTTLSeconds: 300,
		Endpoints: []model.ProviderEndpoint{{
			ID:       providerID + "-endpoint",
			Label:    "primary",
			BaseURL:  strings.TrimSpace(integration.BaseURL),
			Enabled:  true,
			Weight:   1,
			Priority: 0,
			Headers:  map[string]string{},
		}},
		Credentials: []model.ProviderCredential{{
			ID:      providerID + "-credential",
			Label:   "relay",
			APIKey:  apiKey,
			Enabled: true,
			Weight:  1,
			Headers: map[string]string{},
		}},
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:             pointerBool(true),
			FailureThreshold:    3,
			CooldownSeconds:     60,
			HalfOpenMaxRequests: 1,
		},
		UpdatedAt: now,
	}
}

func upsertProvider(providers *[]model.Provider, next model.Provider) {
	for i := range *providers {
		if (*providers)[i].ID == next.ID {
			next.CreatedAt = (*providers)[i].CreatedAt
			(*providers)[i] = next
			return
		}
	}
	next.CreatedAt = time.Now().UTC()
	*providers = append(*providers, next)
}

const autoCreatedIntegrationRouteNote = "auto-created from integration sync"

func syncAutoRoutes(state *model.State, integration model.Integration, snapshot model.IntegrationSnapshot) {
	providerID := strings.TrimSpace(integration.LinkedProviderID)
	if providerID == "" {
		return
	}
	desiredModels := make(map[string]string, len(snapshot.ModelNames))
	for _, modelName := range snapshot.ModelNames {
		alias := strings.TrimSpace(modelName)
		if alias == "" {
			continue
		}
		desiredModels[strings.ToLower(alias)] = alias

		target := model.RouteTarget{
			ID:               model.NewID("target"),
			AccountID:        providerID,
			UpstreamModel:    modelName,
			Weight:           1,
			Enabled:          true,
			MarkupMultiplier: integrationMarkup(integration, modelName),
		}

		found := false
		for routeIndex := range state.ModelRoutes {
			route := &state.ModelRoutes[routeIndex]
			if !strings.EqualFold(route.Alias, alias) {
				continue
			}
			found = true
			exists := false
			for targetIndex, current := range route.Targets {
				if current.AccountID == target.AccountID && current.UpstreamModel == target.UpstreamModel {
					route.Targets[targetIndex].Enabled = true
					route.Targets[targetIndex].Weight = 1
					route.Targets[targetIndex].MarkupMultiplier = target.MarkupMultiplier
					exists = true
					break
				}
			}
			if !exists {
				route.Targets = append(route.Targets, target)
			}
			break
		}
		if !found {
			state.ModelRoutes = append(state.ModelRoutes, model.ModelRoute{
				Alias:   alias,
				Targets: []model.RouteTarget{target},
				Notes:   autoCreatedIntegrationRouteNote,
			})
		}
	}

	removedAliases := map[string]struct{}{}
	filteredRoutes := state.ModelRoutes[:0]
	for _, route := range state.ModelRoutes {
		filteredTargets := route.Targets[:0]
		for _, target := range route.Targets {
			if target.AccountID == providerID {
				if _, ok := desiredModels[strings.ToLower(strings.TrimSpace(target.UpstreamModel))]; !ok {
					continue
				}
			}
			filteredTargets = append(filteredTargets, target)
		}
		route.Targets = slices.Clone(filteredTargets)
		if len(route.Targets) == 0 && len(route.Scenarios) == 0 && strings.TrimSpace(route.Notes) == autoCreatedIntegrationRouteNote {
			removedAliases[strings.ToLower(strings.TrimSpace(route.Alias))] = struct{}{}
			continue
		}
		filteredRoutes = append(filteredRoutes, route)
	}
	state.ModelRoutes = filteredRoutes
	removeGatewayKeyModelReferences(state.GatewayKeys, removedAliases)
}

func integrationMarkup(integration model.Integration, modelName string) float64 {
	if value, ok := integration.ModelMarkupOverrides[modelName]; ok && value > 0 {
		return value
	}
	if integration.DefaultMarkupMultiplier > 0 {
		return integration.DefaultMarkupMultiplier
	}
	return 1
}

func removeGatewayKeyModelReferences(keys []model.GatewayKey, aliases map[string]struct{}) {
	if len(aliases) == 0 {
		return
	}
	for keyIndex := range keys {
		if len(keys[keyIndex].AllowedModels) == 0 {
			continue
		}
		filtered := keys[keyIndex].AllowedModels[:0]
		for _, alias := range keys[keyIndex].AllowedModels {
			if _, removed := aliases[strings.ToLower(strings.TrimSpace(alias))]; removed {
				continue
			}
			filtered = append(filtered, alias)
		}
		keys[keyIndex].AllowedModels = slices.Clone(filtered)
	}
}

func mapsClonePricing(values map[string]model.IntegrationPricing) map[string]model.IntegrationPricing {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]model.IntegrationPricing, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func syncWarningMessage(modelsErr, pricingErr error) string {
	warnings := make([]string, 0, 2)
	if modelsErr != nil {
		warnings = append(warnings, "model inventory sync skipped: "+modelsErr.Error())
	}
	if pricingErr != nil {
		warnings = append(warnings, "pricing sync skipped: "+pricingErr.Error())
	}
	return joinWarnings(warnings...)
}

func joinWarnings(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		filtered = append(filtered, part)
	}
	return strings.Join(filtered, "; ")
}

func syncManagedPricingProfiles(state *model.State, integration model.Integration, snapshot model.IntegrationSnapshot) {
	prefix := managedPricingProfilePrefix(integration.ID)
	desired := map[string]model.PricingProfile{}
	for modelName, pricing := range snapshot.Prices {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			continue
		}
		profileID := managedPricingProfileID(integration.ID, modelName)
		currency := strings.TrimSpace(pricing.Currency)
		if currency == "" {
			currency = strings.TrimSpace(snapshot.Currency)
		}
		if currency == "" {
			currency = "USD"
		}
		desired[profileID] = model.PricingProfile{
			ID:               profileID,
			Name:             integration.Name + " / " + modelName,
			Currency:         currency,
			InputPerMillion:  pricing.InputPerMillion,
			OutputPerMillion: pricing.OutputPerMillion,
		}
	}

	filtered := make([]model.PricingProfile, 0, len(state.PricingProfiles)+len(desired))
	for _, profile := range state.PricingProfiles {
		if strings.HasPrefix(profile.ID, prefix) {
			if next, ok := desired[profile.ID]; ok {
				filtered = append(filtered, next)
				delete(desired, profile.ID)
			}
			continue
		}
		filtered = append(filtered, profile)
	}
	for _, profile := range desired {
		filtered = append(filtered, profile)
	}
	state.PricingProfiles = filtered

	validManagedIDs := map[string]struct{}{}
	for _, profile := range filtered {
		if strings.HasPrefix(profile.ID, prefix) {
			validManagedIDs[profile.ID] = struct{}{}
		}
	}
	for routeIndex := range state.ModelRoutes {
		route := &state.ModelRoutes[routeIndex]
		managedID := managedPricingProfileID(integration.ID, route.Alias)
		if _, ok := validManagedIDs[managedID]; ok {
			if strings.TrimSpace(route.PricingProfileID) == "" || strings.HasPrefix(route.PricingProfileID, prefix) {
				route.PricingProfileID = managedID
			}
			continue
		}
		if strings.HasPrefix(route.PricingProfileID, prefix) {
			route.PricingProfileID = ""
		}
	}
}

func managedPricingProfilePrefix(integrationID string) string {
	return "pricing-sync-" + strings.TrimSpace(integrationID) + "-"
}

func managedPricingProfileID(integrationID, modelName string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(modelName)) {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		slug = "model"
	}
	return fmt.Sprintf("%s%s-%08x", managedPricingProfilePrefix(integrationID), slug, modelNameHash(modelName))
}

func modelNameHash(modelName string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(strings.ToLower(strings.TrimSpace(modelName))))
	return hasher.Sum32()
}

func (s *Service) syncNewAPI(ctx context.Context, integration model.Integration) (syncOutcome, error) {
	headers := newAPIHeaders(integration)
	selfData, err := s.getEnvelope(ctx, integration.BaseURL, "/api/user/self", headers)
	if err != nil {
		if strings.TrimSpace(integration.RelayAPIKey) == "" {
			return syncOutcome{}, err
		}
		return s.syncNewAPIRelayFallback(ctx, integration, err)
	}

	var modelsData map[string]any
	var pricingData map[string]any
	var modelsErr error
	var pricingErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		modelsData, modelsErr = s.getEnvelope(ctx, integration.BaseURL, "/api/user/models", headers)
	}()
	go func() {
		defer wg.Done()
		pricingData, pricingErr = s.getEnvelope(ctx, integration.BaseURL, "/api/pricing", nil)
	}()
	wg.Wait()
	if strings.TrimSpace(integration.RelayAPIKey) != "" && (modelsErr != nil || len(extractModelNames(modelsData)) == 0) {
		relayModels, relayErr := s.getRawJSON(ctx, integration.BaseURL, "/v1/models", relayHeaders(integration))
		if relayErr == nil {
			modelsData = relayModels
			modelsErr = nil
		}
	}

	snapshot := model.IntegrationSnapshot{
		Balance:         readFloat(selfData, "quota"),
		Used:            readFloat(selfData, "used_quota"),
		Currency:        "quota",
		SupportsCheckin: true,
	}
	outcome := syncOutcome{
		Snapshot:         snapshot,
		ModelsRefreshed:  modelsErr == nil,
		PricingRefreshed: pricingErr == nil,
	}
	if outcome.ModelsRefreshed {
		outcome.Snapshot.ModelNames = extractModelNames(modelsData)
		sort.Strings(outcome.Snapshot.ModelNames)
	}
	if outcome.PricingRefreshed {
		outcome.Snapshot.Prices = extractPricingHints(pricingData)
	}
	outcome.Snapshot.LastError = syncWarningMessage(modelsErr, pricingErr)
	return outcome, nil
}

func (s *Service) syncNewAPIRelayFallback(ctx context.Context, integration model.Integration, managementErr error) (syncOutcome, error) {
	modelsData, err := s.getRawJSON(ctx, integration.BaseURL, "/v1/models", relayHeaders(integration))
	if err != nil {
		return syncOutcome{}, fmt.Errorf("management sync failed: %v; relay fallback failed: %w", managementErr, err)
	}

	pricingData, pricingErr := s.getEnvelope(ctx, integration.BaseURL, "/api/pricing", nil)
	snapshot := model.IntegrationSnapshot{
		Balance:         0,
		Used:            0,
		Currency:        "quota",
		ModelNames:      extractModelNames(modelsData),
		SupportsCheckin: false,
	}
	sort.Strings(snapshot.ModelNames)
	outcome := syncOutcome{
		Snapshot:         snapshot,
		ModelsRefreshed:  true,
		PricingRefreshed: pricingErr == nil,
	}
	if outcome.PricingRefreshed {
		outcome.Snapshot.Prices = extractPricingHints(pricingData)
	}
	outcome.Snapshot.LastError = joinWarnings(
		fmt.Sprintf("management API unavailable; relay fallback inventory sync used: %v", managementErr),
		syncWarningMessage(nil, pricingErr),
	)
	return outcome, nil
}

func (s *Service) syncOneHub(ctx context.Context, integration model.Integration) (syncOutcome, error) {
	headers := oneHubHeaders(integration)
	selfData, err := s.getEnvelope(ctx, integration.BaseURL, "/api/user/self", headers)
	if err != nil {
		return syncOutcome{}, err
	}

	var modelsData map[string]any
	var pricingData map[string]any
	var modelsErr error
	var pricingErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		modelsData, modelsErr = s.getRawJSON(ctx, integration.BaseURL, "/v1/models", headers)
	}()
	go func() {
		defer wg.Done()
		pricingData, pricingErr = s.getEnvelope(ctx, integration.BaseURL, "/api/prices?type=db", headers)
	}()
	wg.Wait()

	outcome := syncOutcome{Snapshot: model.IntegrationSnapshot{
		Balance:         readFloat(selfData, "quota"),
		Used:            readFloat(selfData, "used_quota"),
		Currency:        "quota",
		SupportsCheckin: false,
	}}
	outcome.ModelsRefreshed = modelsErr == nil
	outcome.PricingRefreshed = pricingErr == nil
	if outcome.ModelsRefreshed {
		outcome.Snapshot.ModelNames = extractModelNames(modelsData)
		sort.Strings(outcome.Snapshot.ModelNames)
	}
	if outcome.PricingRefreshed {
		outcome.Snapshot.Prices = extractPricingHints(pricingData)
	}
	outcome.Snapshot.LastError = syncWarningMessage(modelsErr, pricingErr)
	return outcome, nil
}

func (s *Service) checkinNewAPI(ctx context.Context, integration model.Integration) (string, *time.Time, error) {
	headers := newAPIHeaders(integration)
	statusData, err := s.getEnvelope(ctx, integration.BaseURL, "/api/user/checkin", headers)
	if err == nil && (readNestedBool(statusData, "stats", "is_checked_in") || readNestedBool(statusData, "stats", "checked_in_today") || readNestedBool(statusData, "stats", "checkedInToday")) {
		now := time.Now().UTC()
		return "already checked in", &now, nil
	}

	respData, err := s.postEnvelope(ctx, integration.BaseURL, "/api/user/checkin", headers, map[string]any{})
	if err != nil {
		return "checkin failed", nil, err
	}
	now := time.Now().UTC()
	if awarded := readFloat(respData, "quota_awarded"); awarded > 0 {
		return fmt.Sprintf("checkin ok, quota_awarded=%.0f", awarded), &now, nil
	}
	return "checkin ok", &now, nil
}

func (s *Service) checkinOneHub(ctx context.Context, integration model.Integration) (string, *time.Time, error) {
	headers := oneHubHeaders(integration)
	_, err := s.postEnvelope(ctx, integration.BaseURL, "/api/user/checkin", headers, map[string]any{})
	if err != nil {
		return "checkin unsupported", nil, err
	}
	now := time.Now().UTC()
	return "checkin ok", &now, nil
}

func newAPIHeaders(integration model.Integration) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + strings.TrimSpace(integration.AccessKey),
		"New-Api-User":  strings.TrimSpace(integration.UserID),
	}
}

func relayHeaders(integration model.Integration) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + strings.TrimSpace(integration.RelayAPIKey),
	}
}

func oneHubHeaders(integration model.Integration) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + strings.TrimSpace(integration.AccessKey),
	}
}

func (s *Service) getEnvelope(ctx context.Context, baseURL, rawPath string, headers map[string]string) (map[string]any, error) {
	payload, err := s.getRawJSON(ctx, baseURL, rawPath, headers)
	if err != nil {
		return nil, err
	}
	return unwrapData(payload), nil
}

func (s *Service) postEnvelope(ctx context.Context, baseURL, rawPath string, headers map[string]string, body any) (map[string]any, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return s.doJSONRequest(ctx, http.MethodPost, baseURL, rawPath, headers, bodyBytes)
}

func (s *Service) getRawJSON(ctx context.Context, baseURL, rawPath string, headers map[string]string) (map[string]any, error) {
	return s.doJSONRequest(ctx, http.MethodGet, baseURL, rawPath, headers, nil)
}

func (s *Service) doJSONRequest(ctx context.Context, method, baseURL, rawPath string, headers map[string]string, body []byte) (map[string]any, error) {
	resolved, err := s.resolveBaseURL(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	reqURL, err := joinURL(resolved.BaseURL, rawPath)
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	applyHeaders(req.Header, headers)

	resp, err := s.clientForResolved(resolved).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := readLimitedResponseBody(resp.Body, maxIntegrationResponseBodyBytes)
	if err != nil {
		if errors.Is(err, errIntegrationResponseTooLarge) {
			return nil, fmt.Errorf("integration response exceeds %d bytes", maxIntegrationResponseBodyBytes)
		}
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("integration request failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if success, exists := payload["success"]; exists && success == false {
		return nil, errors.New(readString(payload, "message"))
	}
	return payload, nil
}

func readLimitedResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errIntegrationResponseTooLarge
	}
	return data, nil
}

func (s *Service) clientForResolved(resolved resolvedBaseURL) *http.Client {
	dialAddress := strings.TrimSpace(resolved.DialAddress)
	clientKey := strings.TrimSpace(resolved.ClientKey)
	if dialAddress == "" || clientKey == "" {
		return s.client
	}

	s.pinnedClientsMu.Lock()
	defer s.pinnedClientsMu.Unlock()

	if client, ok := s.pinnedClients[clientKey]; ok {
		if client.DialAddress == dialAddress && client.Client != nil {
			return client.Client
		}
		if client.Transport != nil {
			client.Transport.CloseIdleConnections()
		}
		delete(s.pinnedClients, clientKey)
	}

	baseTransport := s.transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		baseTransport = baseTransport.Clone()
	}
	baseTransport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, dialAddress)
	}

	timeout := 20 * time.Second
	if s.client != nil && s.client.Timeout > 0 {
		timeout = s.client.Timeout
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: baseTransport,
	}
	s.pinnedClients[clientKey] = pinnedClient{
		DialAddress: dialAddress,
		Client:      client,
		Transport:   baseTransport,
	}
	return client
}

func applyHeaders(dst http.Header, values map[string]string) {
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		dst.Set(key, value)
	}
}

func joinURL(baseURL, rawPath string) (string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", err
	}

	path, rawQuery, _ := strings.Cut(rawPath, "?")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	basePath := strings.TrimRight(base.Path, "/")
	switch {
	case strings.HasSuffix(basePath, "/api") && strings.HasPrefix(path, "/api/"):
		base.Path = basePath + strings.TrimPrefix(path, "/api")
	case strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(path, "/v1/"):
		base.Path = basePath + strings.TrimPrefix(path, "/v1")
	default:
		base.Path = basePath + path
	}
	if strings.TrimSpace(rawQuery) != "" {
		base.RawQuery = strings.TrimSpace(rawQuery)
	}
	return base.String(), nil
}

func unwrapData(payload map[string]any) map[string]any {
	if payload == nil {
		return map[string]any{}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		return data
	}
	return payload
}

func readFloat(values map[string]any, key string) float64 {
	if values == nil {
		return 0
	}
	switch value := values[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		v, _ := value.Float64()
		return v
	}
	return 0
}

func readString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

func readNestedBool(values map[string]any, keys ...string) bool {
	current := any(values)
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = next[key]
		if !ok {
			return false
		}
	}
	value, ok := current.(bool)
	return ok && value
}

func extractModelNames(payload map[string]any) []string {
	type queueItem struct{ value any }

	queue := []queueItem{{value: payload}}
	seen := map[string]bool{}
	models := []string{}
	head := 0

	for head < len(queue) {
		current := queue[head].value
		head++

		switch value := current.(type) {
		case map[string]any:
			if id, ok := value["id"].(string); ok && id != "" {
				appendModelNameCandidate(id, seen, &models)
			}
			if name, ok := value["model"].(string); ok && name != "" {
				appendModelNameCandidate(name, seen, &models)
			}
			if name, ok := value["model_name"].(string); ok && name != "" {
				appendModelNameCandidate(name, seen, &models)
			}
			if name, ok := value["name"].(string); ok && name != "" {
				appendModelNameCandidate(name, seen, &models)
			}
			for _, nested := range value {
				queue = append(queue, queueItem{value: nested})
			}
		case []any:
			for _, nested := range value {
				queue = append(queue, queueItem{value: nested})
			}
		case []string:
			for _, item := range value {
				appendModelNameCandidate(item, seen, &models)
			}
		}
	}

	slices.Sort(models)
	return models
}

func appendModelNameCandidate(raw string, seen map[string]bool, models *[]string) {
	candidate := strings.TrimSpace(raw)
	if !isLikelyModelName(candidate) || seen[candidate] {
		return
	}
	seen[candidate] = true
	*models = append(*models, candidate)
}

func isLikelyModelName(value string) bool {
	if value == "" {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(value), "http://") || strings.HasPrefix(strings.ToLower(value), "https://") {
		return false
	}
	if looksLikeUUID(value) {
		return false
	}
	if len(value) == len("2024-01-15") {
		dateLike := true
		for index, char := range value {
			switch index {
			case 4, 7:
				if char != '-' {
					dateLike = false
				}
			default:
				if char < '0' || char > '9' {
					dateLike = false
				}
			}
		}
		if dateLike {
			return false
		}
	}
	validRune := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z':
			validRune = true
		case char >= 'A' && char <= 'Z':
			validRune = true
		case char >= '0' && char <= '9':
		case char == '-', char == '_', char == '.', char == '/', char == ':':
		default:
			return false
		}
	}
	return validRune
}

func looksLikeUUID(value string) bool {
	if len(value) != len("00000000-0000-0000-0000-000000000000") {
		return false
	}
	hexRunes := 0
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			switch {
			case char >= '0' && char <= '9':
				hexRunes++
			case char >= 'a' && char <= 'f':
				hexRunes++
			case char >= 'A' && char <= 'F':
				hexRunes++
			default:
				return false
			}
		}
	}
	return hexRunes == 32
}

func pointerBool(value bool) *bool {
	v := value
	return &v
}

func (s *Service) resolveBaseURL(ctx context.Context, baseURL string) (resolvedBaseURL, error) {
	trimmedBaseURL := strings.TrimSpace(baseURL)
	parsed, err := url.Parse(trimmedBaseURL)
	if err != nil {
		return resolvedBaseURL{}, fmt.Errorf("invalid integration base_url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return resolvedBaseURL{}, errors.New("integration base_url must use http or https")
	}

	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return resolvedBaseURL{}, errors.New("integration base_url host is required")
	}
	lowerHost := strings.ToLower(host)
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") {
		return resolvedBaseURL{}, errors.New("integration base_url must not target localhost")
	}

	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}

	if ip := net.ParseIP(host); ip != nil {
		if !s.allowPrivateBaseURLs && blockedBaseURLIP(ip) {
			return resolvedBaseURL{}, errors.New("integration base_url must not target private or local addresses")
		}
		return resolvedBaseURL{
			BaseURL:     trimmedBaseURL,
			ClientKey:   resolvedBaseURLClientKey(parsed),
			DialAddress: net.JoinHostPort(ip.String(), port),
		}, nil
	}

	lookupIPs := s.lookupIPs
	if lookupIPs == nil {
		lookupIPs = net.DefaultResolver.LookupIP
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := lookupIPs(ctx, "ip", host)
	if err != nil {
		return resolvedBaseURL{}, fmt.Errorf("invalid integration base_url: cannot resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return resolvedBaseURL{}, fmt.Errorf("invalid integration base_url: host %q resolved to no addresses", host)
	}
	if !s.allowPrivateBaseURLs {
		for _, ip := range ips {
			if blockedBaseURLIP(ip) {
				return resolvedBaseURL{}, errors.New("integration base_url must not target private or local addresses")
			}
		}
	}
	return resolvedBaseURL{
		BaseURL:     trimmedBaseURL,
		ClientKey:   resolvedBaseURLClientKey(parsed),
		DialAddress: net.JoinHostPort(ips[0].String(), port),
	}, nil
}

func resolvedBaseURLClientKey(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	port := resolvedBaseURLEffectivePort(parsed)
	if scheme == "" || host == "" || port == "" {
		return ""
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func resolvedBaseURLEffectivePort(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	if port := strings.TrimSpace(parsed.Port()); port != "" {
		return port
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func blockedBaseURLIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func extractPricingHints(payload map[string]any) map[string]model.IntegrationPricing {
	result := map[string]model.IntegrationPricing{}

	var walk func(any)
	walk = func(current any) {
		switch value := current.(type) {
		case map[string]any:
			name := readString(value, "model")
			if name == "" {
				name = readString(value, "model_name")
			}
			if name == "" {
				name = readString(value, "name")
			}
			input := readFloat(value, "input_per_million")
			output := readFloat(value, "output_per_million")
			if name != "" && (input > 0 || output > 0) {
				result[name] = model.IntegrationPricing{
					InputPerMillion:  input,
					OutputPerMillion: output,
					Currency:         readString(value, "currency"),
				}
			}
			for _, nested := range value {
				walk(nested)
			}
		case []any:
			for _, nested := range value {
				walk(nested)
			}
		}
	}

	walk(payload)
	return result
}

func sameDay(value *time.Time, now time.Time) bool {
	if value == nil {
		return false
	}
	y1, m1, d1 := value.UTC().Date()
	y2, m2, d2 := now.UTC().Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

func NeedsCheckin(integration model.Integration, now time.Time) bool {
	if !integration.Enabled || !integration.Snapshot.SupportsCheckin {
		return false
	}
	return !sameDay(integration.Snapshot.LastCheckinAt, now)
}

func findIntegrationIndex(integrations []model.Integration, integrationID string) int {
	return slices.IndexFunc(integrations, func(integration model.Integration) bool {
		return integration.ID == integrationID
	})
}
