package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/KoinaAI/conduit/backend/internal/billing"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

type Service struct {
	cfg             config.Config
	store           *store.FileStore
	httpClient      *http.Client
	transport       *http.Transport
	websocketDialer *websocket.Dialer
	runtime         *runtimeState
}

type resolvedCandidate struct {
	provider     model.Provider
	route        model.ModelRoute
	target       model.RouteTarget
	pricing      model.PricingProfile
	endpoint     model.ProviderEndpoint
	credential   model.ProviderCredential
	multiplier   float64
	gatewayKeyID string
	sessionID    string
}

type parsedProxyRequest struct {
	routeAlias  string
	stream      bool
	rawBody     []byte
	jsonPayload map[string]any
	sessionID   string
}

type upstreamExchange struct {
	Path          string
	Body          []byte
	Stream        bool
	ResponseMode  responseMode
	UpstreamModel string
	PublicAlias   string
}

type responseMode string

const (
	responseModePassthrough   responseMode = "passthrough"
	responseModeAnthropicJSON responseMode = "anthropic-json"
	responseModeAnthropicSSE  responseMode = "anthropic-sse"
	responseModeGeminiJSON    responseMode = "gemini-json"
	responseModeGeminiSSE     responseMode = "gemini-sse"
	maxRealtimeFrameBytes     int64        = 8 << 20
)

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

type openAIChoiceDelta struct {
	Content string `json:"content"`
}

type openAIMessage struct {
	Content any `json:"content"`
}

type openAIChoice struct {
	Message      openAIMessage     `json:"message"`
	Delta        openAIChoiceDelta `json:"delta"`
	FinishReason string            `json:"finish_reason"`
}

type openAIChatResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
}

func NewService(cfg config.Config, store *store.FileStore) *Service {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Service{
		cfg:   cfg,
		store: store,
		httpClient: &http.Client{
			Transport: transport,
		},
		transport: transport,
		websocketDialer: &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 10 * time.Second,
			NetDialContext:   (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		},
		runtime: newRuntimeState(),
	}
}

// HandleModels serves GET /v1/models and filters aliases by the authenticated
// consumer key.
func (s *Service) HandleModels(w http.ResponseWriter, r *http.Request) {
	state := s.store.RoutingSnapshot()
	gatewayKey, err := s.authenticateGatewayRequest(state, r.Header, "", "")
	if err != nil {
		writeGatewayAuthError(w, err)
		return
	}
	defer s.runtime.releaseGatewayKey(gatewayKey.ID, 0)

	items := make([]map[string]any, 0, len(state.ModelRoutes))
	for _, route := range state.ModelRoutes {
		if !gatewayKey.AllowsModel(route.Alias) {
			continue
		}
		items = append(items, map[string]any{
			"id":       route.Alias,
			"object":   "model",
			"created":  routeAliasTimestamp(route.Alias),
			"owned_by": "universal-ai-gateway",
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

// ProxyHTTP handles all HTTP-based gateway protocols.
func (s *Service) ProxyHTTP(protocol model.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqBody, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("failed to read request body: %v", err))
			return
		}
		defer r.Body.Close()

		parsedRequest, err := parseProxyRequest(protocol, r, reqBody)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		state := s.store.RoutingSnapshot()
		gatewayKey, err := s.authenticateGatewayRequest(state, r.Header, protocol, parsedRequest.routeAlias)
		if err != nil {
			writeGatewayAuthError(w, err)
			return
		}
		var finalCost float64
		defer func() {
			s.runtime.releaseGatewayKey(gatewayKey.ID, finalCost)
		}()

		candidates, _, pricing, err := s.buildCandidatePlan(state, parsedRequest.routeAlias, protocol, gatewayKey.ID, parsedRequest.sessionID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		started := time.Now().UTC()
		record := model.RequestRecord{
			ID:              model.NewID("req"),
			Protocol:        protocol,
			RouteAlias:      parsedRequest.routeAlias,
			GatewayKeyID:    gatewayKey.ID,
			ClientSessionID: parsedRequest.sessionID,
			Path:            r.URL.Path,
			StartedAt:       started,
			Stream:          parsedRequest.stream,
		}

		attempts := make([]model.RequestAttemptRecord, 0, len(candidates))
		var lastErr error
		var lastStatusCode int

		for index, candidate := range candidates {
			exchange, err := prepareUpstreamExchange(protocol, candidate.provider.Kind, r.URL.Path, parsedRequest, effectiveUpstreamModel(candidate))
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			candidate.gatewayKeyID = gatewayKey.ID
			candidate.sessionID = parsedRequest.sessionID

			attemptStarted := time.Now().UTC()
			resp, err := s.doProxyRequest(r.Context(), candidate, exchange, r.Method, r.Header)
			if err != nil {
				lastErr = err
				attempts = append(attempts, model.RequestAttemptRecord{
					RequestID:     record.ID,
					Sequence:      index + 1,
					ProviderID:    candidate.provider.ID,
					ProviderName:  candidate.provider.Name,
					EndpointID:    candidate.endpoint.ID,
					EndpointURL:   candidate.endpoint.BaseURL,
					CredentialID:  candidate.credential.ID,
					UpstreamModel: exchange.UpstreamModel,
					StatusCode:    0,
					Retryable:     true,
					Decision:      "switch_provider",
					StartedAt:     attemptStarted,
					DurationMS:    time.Since(attemptStarted).Milliseconds(),
					Error:         err.Error(),
				})
				s.runtime.reportFailure(candidate, 0, err.Error(), 0)
				if index < len(candidates)-1 {
					sleepBackoff(0, index+1)
					continue
				}
				break
			}

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
				_ = resp.Body.Close()
				lastStatusCode = resp.StatusCode
				lastErr = fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
				retryable := isRetryableStatus(resp.StatusCode)
				retryAfter := parseRetryAfter(resp.Header)
				attempts = append(attempts, model.RequestAttemptRecord{
					RequestID:     record.ID,
					Sequence:      index + 1,
					ProviderID:    candidate.provider.ID,
					ProviderName:  candidate.provider.Name,
					EndpointID:    candidate.endpoint.ID,
					EndpointURL:   candidate.endpoint.BaseURL,
					CredentialID:  candidate.credential.ID,
					UpstreamModel: exchange.UpstreamModel,
					StatusCode:    resp.StatusCode,
					Retryable:     retryable,
					Decision:      nextDecision(retryable, index < len(candidates)-1),
					StartedAt:     attemptStarted,
					DurationMS:    time.Since(attemptStarted).Milliseconds(),
					Error:         strings.TrimSpace(string(body)),
				})
				s.runtime.reportFailure(candidate, resp.StatusCode, strings.TrimSpace(string(body)), retryAfter)
				if retryable && index < len(candidates)-1 {
					sleepBackoff(resp.StatusCode, index+1)
					continue
				}
				break
			}

			record.AccountID = candidate.provider.ID
			record.ProviderName = candidate.provider.Name
			record.UpstreamModel = exchange.UpstreamModel
			record.StatusCode = resp.StatusCode
			record.AttemptCount = index + 1

			observer := NewUsageObserver(protocol)
			setBillingTrailers(w.Header())
			writeErr := s.writeProxyResponse(w, resp, observer, exchange, parsedRequest.routeAlias)
			_ = resp.Body.Close()
			if writeErr != nil {
				lastErr = writeErr
				retryable := !responseWriteStarted(writeErr)
				hasNext := index < len(candidates)-1
				lastStatusCode = http.StatusBadGateway
				attempts = append(attempts, model.RequestAttemptRecord{
					RequestID:     record.ID,
					Sequence:      index + 1,
					ProviderID:    candidate.provider.ID,
					ProviderName:  candidate.provider.Name,
					EndpointID:    candidate.endpoint.ID,
					EndpointURL:   candidate.endpoint.BaseURL,
					CredentialID:  candidate.credential.ID,
					UpstreamModel: exchange.UpstreamModel,
					StatusCode:    http.StatusBadGateway,
					Retryable:     retryable,
					Decision:      nextDecision(retryable, hasNext),
					StartedAt:     attemptStarted,
					DurationMS:    time.Since(attemptStarted).Milliseconds(),
					Error:         writeErr.Error(),
				})
				s.runtime.reportFailure(candidate, 0, writeErr.Error(), 0)
				if retryable && hasNext {
					sleepBackoff(0, index+1)
					continue
				}
				break
			}

			record.DurationMS = time.Since(started).Milliseconds()
			record.Usage = observer.Summary()
			record.Billing = billing.Calculate(pricing, record.Usage, candidate.multiplier, pricing.Name)
			finalCost = record.Billing.FinalCost
			writeBillingMetadata(w.Header(), record)
			attempts = append(attempts, model.RequestAttemptRecord{
				RequestID:     record.ID,
				Sequence:      index + 1,
				ProviderID:    candidate.provider.ID,
				ProviderName:  candidate.provider.Name,
				EndpointID:    candidate.endpoint.ID,
				EndpointURL:   candidate.endpoint.BaseURL,
				CredentialID:  candidate.credential.ID,
				UpstreamModel: exchange.UpstreamModel,
				StatusCode:    resp.StatusCode,
				Retryable:     false,
				Decision:      "success",
				StartedAt:     attemptStarted,
				DurationMS:    time.Since(attemptStarted).Milliseconds(),
			})
			s.runtime.reportSuccess(candidate, time.Since(attemptStarted))
			s.appendRecord(record, attempts)
			return
		}

		record.StatusCode = http.StatusBadGateway
		if lastStatusCode > 0 {
			record.StatusCode = lastStatusCode
		}
		record.AttemptCount = len(attempts)
		record.DurationMS = time.Since(started).Milliseconds()
		if lastErr != nil {
			record.Error = lastErr.Error()
		}
		s.appendRecord(record, attempts)
		if responseWriteStarted(lastErr) {
			return
		}
		writeError(w, record.StatusCode, record.Error)
	}
}

// ProxyRealtime proxies WebSocket traffic to OpenAI-compatible realtime APIs.
func (s *Service) ProxyRealtime(w http.ResponseWriter, r *http.Request) {
	routeAlias := strings.TrimSpace(r.URL.Query().Get("model"))
	if routeAlias == "" {
		writeError(w, http.StatusBadRequest, "model query parameter is required")
		return
	}

	state := s.store.RoutingSnapshot()
	gatewayKey, err := s.authenticateGatewayRequest(state, r.Header, model.ProtocolOpenAIRealtime, routeAlias)
	if err != nil {
		writeGatewayAuthError(w, err)
		return
	}
	var finalCost float64
	defer func() {
		s.runtime.releaseGatewayKey(gatewayKey.ID, finalCost)
	}()

	candidates, _, pricing, err := s.buildCandidatePlan(state, routeAlias, model.ProtocolOpenAIRealtime, gatewayKey.ID, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(candidates) == 0 {
		writeError(w, http.StatusBadGateway, "no realtime candidate available")
		return
	}
	candidate := candidates[0]
	upstreamURL, err := joinProxyURL(candidate.endpoint.BaseURL, "/v1/realtime")
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	switch upstreamURL.Scheme {
	case "https":
		upstreamURL.Scheme = "wss"
	case "http", "":
		upstreamURL.Scheme = "ws"
	}
	query := r.URL.Query()
	query.Set("model", effectiveUpstreamModel(candidate))
	upstreamURL.RawQuery = query.Encode()

	upgrader := websocket.Upgrader{CheckOrigin: s.allowRealtimeOrigin}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientConn.Close()
	clientConn.SetReadLimit(maxRealtimeFrameBytes)

	headers := http.Header{}
	copyForwardHeaders(headers, r.Header)
	applyProviderAuth(headers, candidate.provider.Kind, candidate.credential.APIKey)
	for key, value := range candidate.provider.Headers {
		headers.Set(key, value)
	}
	for key, value := range candidate.endpoint.Headers {
		headers.Set(key, value)
	}
	for key, value := range candidate.credential.Headers {
		headers.Set(key, value)
	}

	serverConn, _, err := s.websocketDialer.DialContext(r.Context(), upstreamURL.String(), headers)
	if err != nil {
		_ = clientConn.WriteJSON(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	defer serverConn.Close()
	serverConn.SetReadLimit(maxRealtimeFrameBytes)

	started := time.Now().UTC()
	observer := NewUsageObserver(model.ProtocolOpenAIRealtime)
	record := model.RequestRecord{
		ID:            model.NewID("req"),
		Protocol:      model.ProtocolOpenAIRealtime,
		RouteAlias:    routeAlias,
		AccountID:     candidate.provider.ID,
		ProviderName:  candidate.provider.Name,
		UpstreamModel: effectiveUpstreamModel(candidate),
		GatewayKeyID:  gatewayKey.ID,
		StatusCode:    http.StatusSwitchingProtocols,
		StartedAt:     started,
		Stream:        true,
	}

	errCh := make(chan error, 2)
	go proxyWSFrames(serverConn, clientConn, observer, errCh)
	go proxyWSFrames(clientConn, serverConn, nil, errCh)

	firstErr := <-errCh
	_ = serverConn.Close()
	_ = clientConn.Close()
	secondErr := <-errCh
	err = firstErr
	if isNormalClose(err) && !isNormalClose(secondErr) {
		err = secondErr
	}
	record.DurationMS = time.Since(started).Milliseconds()
	record.Usage = observer.Summary()
	record.Billing = billing.Calculate(pricing, record.Usage, candidate.multiplier, pricing.Name)
	finalCost = record.Billing.FinalCost
	if err != nil && !isNormalClose(err) {
		record.Error = err.Error()
	}
	s.appendRecord(record, []model.RequestAttemptRecord{{
		RequestID:     record.ID,
		Sequence:      1,
		ProviderID:    candidate.provider.ID,
		ProviderName:  candidate.provider.Name,
		EndpointID:    candidate.endpoint.ID,
		EndpointURL:   candidate.endpoint.BaseURL,
		CredentialID:  candidate.credential.ID,
		UpstreamModel: record.UpstreamModel,
		StatusCode:    http.StatusSwitchingProtocols,
		Retryable:     false,
		Decision:      "success",
		StartedAt:     started,
		DurationMS:    record.DurationMS,
		Error:         record.Error,
	}})
}

func (s *Service) doProxyRequest(ctx context.Context, candidate resolvedCandidate, exchange upstreamExchange, method string, incoming http.Header) (*http.Response, error) {
	upstreamURL, err := joinProxyURL(candidate.endpoint.BaseURL, exchange.Path)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(candidate.provider.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(reqCtx, method, upstreamURL.String(), bytes.NewReader(exchange.Body))
	if err != nil {
		cancel()
		return nil, err
	}
	copyForwardHeaders(req.Header, incoming)
	applyProviderAuth(req.Header, candidate.provider.Kind, candidate.credential.APIKey)
	for key, value := range candidate.provider.Headers {
		req.Header.Set(key, value)
	}
	for key, value := range candidate.endpoint.Headers {
		req.Header.Set(key, value)
	}
	for key, value := range candidate.credential.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(exchange.Body))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelReadCloser{
		ReadCloser: resp.Body,
		cancel:     cancel,
	}
	return resp, nil
}

func (s *Service) appendRecord(record model.RequestRecord, attempts []model.RequestAttemptRecord) {
	if record.Billing.Currency == "" {
		record.Billing.Currency = "USD"
	}
	_ = s.store.AppendRequestRecord(record, attempts, s.cfg.RequestHistory)
}

// RunProbes actively checks configured endpoints so the admin plane can inspect
// reachability, auth failures, and current passive-circuit status.
func (s *Service) RunProbes(ctx context.Context) map[string]any {
	state := s.store.RoutingSnapshot()
	checkedAt := time.Now().UTC()
	results := make([]map[string]any, 0)

	for _, provider := range state.Providers {
		if !provider.Enabled {
			continue
		}
		for _, endpoint := range provider.Endpoints {
			if !endpoint.Enabled || strings.TrimSpace(endpoint.BaseURL) == "" {
				continue
			}

			result := map[string]any{
				"provider_id":   provider.ID,
				"provider_name": provider.Name,
				"endpoint_id":   endpoint.ID,
				"base_url":      endpoint.BaseURL,
				"checked_at":    checkedAt,
			}

			credential, ok := firstEnabledCredential(provider)
			if !ok {
				result["status"] = "skipped"
				result["error"] = "no enabled credential configured"
				results = append(results, result)
				continue
			}

			path := probePath(provider, endpoint)
			started := time.Now()
			req, err := s.newProbeRequest(ctx, provider, endpoint, credential, path)
			if err != nil {
				result["status"] = "error"
				result["error"] = err.Error()
				results = append(results, result)
				continue
			}

			resp, err := s.httpClient.Do(req)
			durationMS := time.Since(started).Milliseconds()
			result["duration_ms"] = durationMS
			result["credential_id"] = credential.ID
			result["path"] = path
			if err != nil {
				result["status"] = "down"
				result["error"] = err.Error()
				results = append(results, result)
				continue
			}

			result["status_code"] = resp.StatusCode
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				result["status"] = "ok"
			} else {
				result["status"] = "degraded"
			}
			results = append(results, result)
		}
	}

	return map[string]any{
		"checked_at": checkedAt,
		"results":    results,
	}
}

func (s *Service) buildCandidatePlan(state model.RoutingState, alias string, protocol model.Protocol, gatewayKeyID, sessionID string) ([]resolvedCandidate, model.ModelRoute, model.PricingProfile, error) {
	route, ok := state.FindRoute(alias)
	if !ok {
		return nil, model.ModelRoute{}, model.PricingProfile{}, fmt.Errorf("route %q is not configured", alias)
	}
	profile, ok := state.FindPricingProfile(route.PricingProfileID)
	if !ok {
		profile = model.PricingProfile{
			ID:       "default",
			Name:     "default",
			Currency: "USD",
		}
	}

	now := time.Now().UTC()
	healthy := []resolvedCandidate{}
	degraded := []resolvedCandidate{}
	for _, target := range route.Targets {
		if !target.Enabled || !target.Supports(protocol) {
			continue
		}
		provider, ok := state.FindProvider(target.AccountID)
		if !ok || !provider.Enabled || !provider.Supports(protocol) {
			continue
		}
		for _, endpoint := range provider.Endpoints {
			if !endpoint.Enabled || strings.TrimSpace(endpoint.BaseURL) == "" {
				continue
			}
			for _, credential := range provider.Credentials {
				if !credential.Enabled || strings.TrimSpace(credential.APIKey) == "" {
					continue
				}
				multiplier := provider.DefaultMarkupMultiplier * target.MarkupMultiplier
				if multiplier <= 0 {
					multiplier = 1
				}
				candidate := resolvedCandidate{
					provider:     provider,
					route:        route,
					target:       target,
					pricing:      profile,
					endpoint:     endpoint,
					credential:   credential,
					multiplier:   multiplier,
					gatewayKeyID: gatewayKeyID,
					sessionID:    sessionID,
				}
				if s.runtime.endpointOpen(candidate, now) || s.runtime.credentialCoolingDown(candidate, now) {
					degraded = append(degraded, candidate)
					continue
				}
				healthy = append(healthy, candidate)
			}
		}
	}
	if len(healthy) == 0 && len(degraded) == 0 {
		return nil, model.ModelRoute{}, model.PricingProfile{}, fmt.Errorf("route %q has no enabled provider for protocol %s", alias, protocol)
	}

	candidates := healthy
	if len(candidates) == 0 {
		candidates = degraded
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftPriority := candidates[i].target.Priority + candidates[i].endpoint.Priority
		rightPriority := candidates[j].target.Priority + candidates[j].endpoint.Priority
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		leftLatency := candidateLatency(s.runtime.endpointLatency(candidates[i]))
		rightLatency := candidateLatency(s.runtime.endpointLatency(candidates[j]))
		if leftLatency != rightLatency {
			return leftLatency < rightLatency
		}
		leftWeight := totalCandidateWeight(candidates[i])
		rightWeight := totalCandidateWeight(candidates[j])
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		return candidates[i].provider.Name < candidates[j].provider.Name
	})

	if binding, ok := s.runtime.stickyBindingFor(gatewayKeyID, alias, sessionID, now); ok {
		for index, candidate := range candidates {
			if candidate.provider.ID == binding.ProviderID && candidate.endpoint.ID == binding.EndpointID && candidate.credential.ID == binding.CredentialID {
				if index > 0 {
					candidates[0], candidates[index] = candidates[index], candidates[0]
				}
				break
			}
		}
	}

	filteredCandidates := make([]resolvedCandidate, 0, len(candidates))
	providerAttempts := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		limit := candidate.provider.MaxAttempts
		if limit > 0 && providerAttempts[candidate.provider.ID] >= limit {
			continue
		}
		filteredCandidates = append(filteredCandidates, candidate)
		providerAttempts[candidate.provider.ID]++
	}
	if len(filteredCandidates) == 0 {
		return nil, model.ModelRoute{}, model.PricingProfile{}, fmt.Errorf("route %q has no candidate within provider retry budgets", alias)
	}
	return filteredCandidates, route, profile, nil
}

func effectiveUpstreamModel(candidate resolvedCandidate) string {
	upstreamModel := strings.TrimSpace(candidate.target.UpstreamModel)
	if upstreamModel == "" {
		return candidate.route.Alias
	}
	return upstreamModel
}

func firstEnabledCredential(provider model.Provider) (model.ProviderCredential, bool) {
	for _, credential := range provider.Credentials {
		if credential.Enabled && strings.TrimSpace(credential.APIKey) != "" {
			return credential, true
		}
	}
	return model.ProviderCredential{}, false
}

func probePath(provider model.Provider, endpoint model.ProviderEndpoint) string {
	if value := strings.TrimSpace(endpoint.HealthcheckPath); value != "" {
		return value
	}
	if value := strings.TrimSpace(provider.HealthcheckPath); value != "" {
		return value
	}
	switch provider.Kind {
	case model.ProviderKindGemini:
		return "/v1beta/models"
	case model.ProviderKindAnthropic:
		return "/"
	default:
		return "/v1/models"
	}
}

func (s *Service) newProbeRequest(ctx context.Context, provider model.Provider, endpoint model.ProviderEndpoint, credential model.ProviderCredential, path string) (*http.Request, error) {
	upstreamURL, err := joinProxyURL(endpoint.BaseURL, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL.String(), nil)
	if err != nil {
		return nil, err
	}
	applyProviderAuth(req.Header, provider.Kind, credential.APIKey)
	for key, value := range provider.Headers {
		req.Header.Set(key, value)
	}
	for key, value := range endpoint.Headers {
		req.Header.Set(key, value)
	}
	for key, value := range credential.Headers {
		req.Header.Set(key, value)
	}
	return req, nil
}

func totalCandidateWeight(candidate resolvedCandidate) int {
	const maxWeight = int(^uint(0) >> 1)
	weights := []int{candidate.provider.Weight, candidate.target.Weight, candidate.endpoint.Weight, candidate.credential.Weight}
	product := 1
	for _, weight := range weights {
		if weight <= 0 {
			return 1
		}
		if product > maxWeight/weight {
			return maxWeight
		}
		product *= weight
	}
	return product
}

func candidateLatency(latency int64) int64 {
	if latency <= 0 {
		return 1<<62 - 1
	}
	return latency
}

func nextDecision(retryable, hasNext bool) string {
	switch {
	case retryable && hasNext:
		return "switch_provider"
	case retryable:
		return "abort"
	default:
		return "abort"
	}
}

func sleepBackoff(statusCode, retryIndex int) {
	delay := retryBackoffDelay(statusCode, retryIndex)
	if delay <= 0 {
		return
	}
	time.Sleep(delay)
}

func retryBackoffDelay(statusCode, retryIndex int) time.Duration {
	if statusCode >= 500 && statusCode < 600 && statusCode != http.StatusRequestTimeout && statusCode != http.StatusTooManyRequests {
		return 100 * time.Millisecond
	}
	if statusCode == 0 || statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests {
		base := time.Duration(retryIndex*80) * time.Millisecond
		if base > 800*time.Millisecond {
			base = 800 * time.Millisecond
		}
		return base
	}
	return 0
}

func isRetryableStatus(statusCode int) bool {
	if statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests {
		return true
	}
	return statusCode >= 500 && statusCode < 600
}

func parseRetryAfter(headers http.Header) time.Duration {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return clampRetryAfterCooldown(time.Duration(seconds) * time.Second)
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	delay := time.Until(at)
	if delay <= 0 {
		return 0
	}
	return clampRetryAfterCooldown(delay)
}

func writeGatewayAuthError(w http.ResponseWriter, err error) {
	switch err {
	case errRateLimit:
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errConcurrencyLimit:
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errDailyBudget:
		writeError(w, http.StatusPaymentRequired, err.Error())
	default:
		writeError(w, http.StatusUnauthorized, errUnauthorized.Error())
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "gateway_error",
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func parseProxyRequest(protocol model.Protocol, r *http.Request, body []byte) (parsedProxyRequest, error) {
	sessionID := extractClientSessionID(r.Header, body)
	switch protocol {
	case model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
		alias, err := extractGeminiModelFromPath(r.URL.Path)
		if err != nil {
			return parsedProxyRequest{}, err
		}
		return parsedProxyRequest{
			routeAlias: alias,
			stream:     protocol == model.ProtocolGeminiStream,
			rawBody:    body,
			sessionID:  sessionID,
		}, nil
	default:
		if len(body) == 0 {
			return parsedProxyRequest{}, errors.New("request body is required")
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return parsedProxyRequest{}, fmt.Errorf("request body must be JSON: %w", err)
		}
		modelValue, ok := payload["model"].(string)
		if !ok || strings.TrimSpace(modelValue) == "" {
			return parsedProxyRequest{}, errors.New("model is required")
		}
		stream := false
		if value, ok := payload["stream"].(bool); ok {
			stream = value
		}
		return parsedProxyRequest{
			routeAlias:  modelValue,
			stream:      stream,
			rawBody:     body,
			jsonPayload: payload,
			sessionID:   sessionID,
		}, nil
	}
}

func extractClientSessionID(headers http.Header, body []byte) string {
	for _, headerName := range []string{"X-Session-ID", "Session-ID"} {
		if value := strings.TrimSpace(headers.Get(headerName)); value != "" {
			return value
		}
	}
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"session_id", "conversation_id", "thread_id", "chat_id", "prompt_cache_key", "previous_response_id"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if metadata, ok := payload["metadata"].(map[string]any); ok {
		if value, ok := metadata["session_id"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if userID, ok := metadata["user_id"].(string); ok {
			const marker = "_session_"
			if index := strings.Index(userID, marker); index >= 0 {
				return strings.TrimSpace(userID[index+len(marker):])
			}
		}
	}
	return ""
}

func extractGeminiModelFromPath(path string) (string, error) {
	parts := strings.Split(path, "/models/")
	if len(parts) != 2 {
		return "", errors.New("invalid Gemini path")
	}
	fragment := parts[1]
	index := strings.Index(fragment, ":")
	if index < 0 {
		return "", errors.New("invalid Gemini model segment")
	}
	return fragment[:index], nil
}

func copyForwardHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "host", "content-length", "authorization", "x-api-key", "connection", "upgrade", "sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions", "sec-websocket-accept":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func applyProviderAuth(headers http.Header, providerKind model.ProviderKind, apiKey string) {
	switch providerKind {
	case model.ProviderKindAnthropic:
		headers.Set("x-api-key", apiKey)
		if headers.Get("anthropic-version") == "" {
			headers.Set("anthropic-version", "2023-06-01")
		}
	case model.ProviderKindGemini:
		headers.Set("x-goog-api-key", apiKey)
	default:
		headers.Set("Authorization", "Bearer "+apiKey)
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func setBillingTrailers(headers http.Header) {
	headers.Add("Trailer", "X-Gateway-Input-Tokens")
	headers.Add("Trailer", "X-Gateway-Output-Tokens")
	headers.Add("Trailer", "X-Gateway-Total-Tokens")
	headers.Add("Trailer", "X-Gateway-Cached-Tokens")
	headers.Add("Trailer", "X-Gateway-Reasoning-Tokens")
	headers.Add("Trailer", "X-Gateway-Billing-Currency")
	headers.Add("Trailer", "X-Gateway-Billing-Base")
	headers.Add("Trailer", "X-Gateway-Billing-Final")
	headers.Add("Trailer", "X-Gateway-Billing-Multiplier")
}

func writeBillingMetadata(headers http.Header, record model.RequestRecord) {
	headers.Set("X-Gateway-Input-Tokens", strconv.FormatInt(record.Usage.InputTokens, 10))
	headers.Set("X-Gateway-Output-Tokens", strconv.FormatInt(record.Usage.OutputTokens, 10))
	headers.Set("X-Gateway-Total-Tokens", strconv.FormatInt(record.Usage.TotalTokens, 10))
	headers.Set("X-Gateway-Cached-Tokens", strconv.FormatInt(record.Usage.CachedInputTokens, 10))
	headers.Set("X-Gateway-Reasoning-Tokens", strconv.FormatInt(record.Usage.ReasoningTokens, 10))
	headers.Set("X-Gateway-Billing-Currency", record.Billing.Currency)
	headers.Set("X-Gateway-Billing-Base", fmt.Sprintf("%.6f", record.Billing.BaseCost))
	headers.Set("X-Gateway-Billing-Final", fmt.Sprintf("%.6f", record.Billing.FinalCost))
	headers.Set("X-Gateway-Billing-Multiplier", fmt.Sprintf("%.4f", record.Billing.Multiplier))
}

func copyStreamingResponse(w http.ResponseWriter, body io.Reader, observer *UsageObserver) error {
	reader := bufio.NewReader(body)
	flusher, _ := w.(http.Flusher)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			observer.ObserveLine(line)
			if _, writeErr := w.Write(line); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func joinProxyURL(baseURL, rawPath string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	path := rawPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	basePath := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(path, "/v1/"):
		parsed.Path = basePath + strings.TrimPrefix(path, "/v1")
	case strings.HasSuffix(basePath, "/v1beta") && strings.HasPrefix(path, "/v1beta/"):
		parsed.Path = basePath + strings.TrimPrefix(path, "/v1beta")
	default:
		parsed.Path = basePath + path
	}
	return parsed, nil
}

func proxyWSFrames(src, dst *websocket.Conn, observer *UsageObserver, errCh chan<- error) {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			errCh <- err
			return
		}
		if observer != nil && (messageType == websocket.TextMessage || messageType == websocket.BinaryMessage) {
			observer.ObserveJSON(payload)
		}
		if err := dst.WriteMessage(messageType, payload); err != nil {
			errCh <- err
			return
		}
	}
}

func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}

func routeAliasTimestamp(alias string) int64 {
	const modelListEpochUnix = 1_775_001_600
	return modelListEpochUnix
}

func (s *Service) allowRealtimeOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	return s.cfg.AllowsRealtimeOrigin(origin)
}
