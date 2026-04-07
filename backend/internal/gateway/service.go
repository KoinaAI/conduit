package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	netproxy "golang.org/x/net/proxy"

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
	proxyMu         sync.Mutex
	proxyClients    map[string]*http.Client
	proxyDialers    map[string]*websocket.Dialer
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
	scenario     string
}

type parsedProxyRequest struct {
	routeAlias  string
	stream      bool
	rawBody     []byte
	rawQuery    string
	jsonPayload map[string]any
	sessionID   string
	scenario    string
}

type upstreamExchange struct {
	Path                string
	Body                []byte
	Stream              bool
	ResponseMode        responseMode
	UpstreamModel       string
	PublicAlias         string
	RequestHeaderSet    http.Header
	RequestHeaderRemove []string
}

const maxProxyRequestBodyBytes int64 = 8 << 20

var errRequestBodyTooLarge = errors.New("request body too large")

type responseMode string

const (
	responseModePassthrough   responseMode = "passthrough"
	responseModeAnthropicJSON responseMode = "anthropic-json"
	responseModeAnthropicSSE  responseMode = "anthropic-sse"
	responseModeGeminiJSON    responseMode = "gemini-json"
	responseModeGeminiSSE     responseMode = "gemini-sse"
	responseModeResponsesJSON responseMode = "responses-json"
	responseModeResponsesSSE  responseMode = "responses-sse"
	maxRealtimeFrameBytes     int64        = 8 << 20
)

const (
	headerCodexTurnState     = "X-Codex-Turn-State"
	defaultRuntimeSessionTTL = 2 * time.Minute
	runtimeSessionHeartbeat  = 30 * time.Second
)

const (
	providerRoutingModeRoundRobin model.ProviderRoutingMode = "round-robin"
	providerRoutingModeRandom     model.ProviderRoutingMode = "random"
	providerRoutingModeFailover   model.ProviderRoutingMode = "failover"
	providerRoutingModeOrdered    model.ProviderRoutingMode = "ordered"
)

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

var gatewayTracer = otel.Tracer("github.com/KoinaAI/conduit/backend/internal/gateway")

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
		runtime:      newRuntimeState(newRedisStickyStore(cfg)),
		proxyClients: map[string]*http.Client{},
		proxyDialers: map[string]*websocket.Dialer{},
	}
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	if s.transport != nil {
		s.transport.CloseIdleConnections()
	}
	s.proxyMu.Lock()
	for _, client := range s.proxyClients {
		if client != nil {
			if transport, ok := client.Transport.(*http.Transport); ok {
				transport.CloseIdleConnections()
			}
		}
	}
	s.proxyMu.Unlock()
	type closer interface {
		Close() error
	}
	if stickyStore, ok := s.runtime.stickyStore.(closer); ok {
		return stickyStore.Close()
	}
	return nil
}

func (s *Service) trackRuntimeSession(session LiveSessionStatus) func() {
	key := ""
	if strings.TrimSpace(session.SessionID) != "" {
		key = s.runtime.startSession(session, defaultRuntimeSessionTTL)
	}
	if key == "" && strings.TrimSpace(session.GatewayKeyID) == "" && strings.TrimSpace(session.ProviderID) == "" {
		return func() {}
	}
	stop := make(chan struct{})
	interval := runtimeSessionHeartbeat
	if defaultRuntimeSessionTTL/2 < interval {
		interval = defaultRuntimeSessionTTL / 2
	}
	if interval <= 0 {
		interval = runtimeSessionHeartbeat
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now().UTC()
				if key != "" {
					s.runtime.touchSession(key, now, defaultRuntimeSessionTTL)
				}
				s.runtime.touchInFlightLeases(session.GatewayKeyID, session.ProviderID, time.Hour)
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			if key != "" {
				s.runtime.endSession(key)
			}
		})
	}
}

// HandleModels serves GET /v1/models and filters aliases by the authenticated
// consumer key.
func (s *Service) HandleModels(w http.ResponseWriter, r *http.Request) {
	state := s.store.RoutingSnapshot()
	gatewayKey, err := s.authenticateGatewayRequest(state, r.Header, "", "", gatewayRequestSource(r))
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
		defer r.Body.Close()
		reqBody, err := readLimitedRequestBody(r.Body, maxProxyRequestBodyBytes)
		if err != nil {
			if errors.Is(err, errRequestBodyTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", maxProxyRequestBodyBytes))
				return
			}
			writeError(w, http.StatusBadRequest, fmt.Sprintf("failed to read request body: %v", err))
			return
		}

		parsedRequest, err := parseProxyRequest(protocol, r, reqBody)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		responseTurnState := codexResponseTurnState(protocol, r.Header)
		ctx, requestSpan := gatewayTracer.Start(r.Context(), "gateway.proxy_http",
			trace.WithAttributes(
				attribute.String("gateway.protocol", string(protocol)),
				attribute.String("http.method", r.Method),
				attribute.String("http.route", r.URL.Path),
				attribute.String("gateway.route_alias", parsedRequest.routeAlias),
			),
		)
		defer requestSpan.End()
		if parsedRequest.sessionID != "" {
			requestSpan.SetAttributes(attribute.String("gateway.session_id", parsedRequest.sessionID))
		}
		if parsedRequest.scenario != "" {
			requestSpan.SetAttributes(attribute.String("gateway.requested_scenario", parsedRequest.scenario))
		}
		r = r.WithContext(ctx)

		state := s.store.RoutingSnapshot()
		_, authSpan := gatewayTracer.Start(ctx, "gateway.authenticate")
		gatewayKey, err := s.authenticateGatewayRequest(state, r.Header, protocol, parsedRequest.routeAlias, gatewayRequestSource(r))
		authSpan.End()
		if err != nil {
			requestSpan.RecordError(err)
			requestSpan.SetStatus(codes.Error, err.Error())
			writeGatewayAuthError(w, err)
			return
		}
		var finalCost float64
		defer func() {
			s.runtime.releaseGatewayKey(gatewayKey.ID, finalCost)
		}()

		_, planSpan := gatewayTracer.Start(ctx, "gateway.route_plan")
		candidates, route, _, appliedScenario, err := s.buildCandidatePlan(state, parsedRequest.routeAlias, protocol, gatewayKey.ID, parsedRequest.sessionID, parsedRequest.scenario)
		planSpan.SetAttributes(attribute.Int("gateway.candidate_count", len(candidates)))
		if appliedScenario != "" {
			planSpan.SetAttributes(attribute.String("gateway.scenario", appliedScenario))
		}
		planSpan.End()
		if err != nil {
			requestSpan.RecordError(err)
			requestSpan.SetStatus(codes.Error, err.Error())
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
		if record.ClientSessionID == "" && responseTurnState != "" {
			record.ClientSessionID = responseTurnState
		}
		liveSessionID := record.ClientSessionID
		record.RoutingDecision = newRoutingDecision(route, appliedScenario, gatewayKey.ID, parsedRequest.sessionID, candidates, s.runtime, started)
		logger := slog.With(
			"request_id", record.ID,
			"protocol", protocol,
			"route_alias", parsedRequest.routeAlias,
			"gateway_key_id", gatewayKey.ID,
		)

		attempts := make([]model.RequestAttemptRecord, 0, len(candidates))
		var lastErr error
		var lastStatusCode int

		for index, candidate := range candidates {
			attemptCtx, attemptSpan := gatewayTracer.Start(ctx, "gateway.upstream_attempt",
				trace.WithAttributes(
					attribute.Int("gateway.attempt", index+1),
					attribute.String("gateway.provider_id", candidate.provider.ID),
					attribute.String("gateway.endpoint_id", candidate.endpoint.ID),
					attribute.String("gateway.credential_id", candidate.credential.ID),
				),
			)
			exchange, err := prepareUpstreamExchange(protocol, candidate.provider.Kind, r.URL.Path, parsedRequest, effectiveUpstreamModel(candidate))
			if err != nil {
				attemptSpan.RecordError(err)
				attemptSpan.SetStatus(codes.Error, err.Error())
				attemptSpan.End()
				requestSpan.RecordError(err)
				requestSpan.SetStatus(codes.Error, err.Error())
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			exchange, err = applyRequestTransformers(exchange, candidate)
			if err != nil {
				attemptSpan.RecordError(err)
				attemptSpan.SetStatus(codes.Error, err.Error())
				attemptSpan.End()
				requestSpan.RecordError(err)
				requestSpan.SetStatus(codes.Error, err.Error())
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			candidate.gatewayKeyID = gatewayKey.ID
			candidate.sessionID = parsedRequest.sessionID

			attemptStarted := time.Now().UTC()
			if err := s.runtime.acquireProvider(candidate.provider, attemptStarted); err != nil {
				attemptSpan.RecordError(err)
				attemptSpan.SetStatus(codes.Error, err.Error())
				attemptSpan.End()
				lastErr = err
				lastStatusCode = providerLimitStatusCode(err)
				hasNext := index < len(candidates)-1
				attempts = append(attempts, model.RequestAttemptRecord{
					RequestID:     record.ID,
					Sequence:      index + 1,
					ProviderID:    candidate.provider.ID,
					ProviderName:  candidate.provider.Name,
					EndpointID:    candidate.endpoint.ID,
					EndpointURL:   candidate.endpoint.BaseURL,
					CredentialID:  candidate.credential.ID,
					UpstreamModel: exchange.UpstreamModel,
					StatusCode:    lastStatusCode,
					Retryable:     true,
					Decision:      nextDecision(true, hasNext),
					StartedAt:     attemptStarted,
					DurationMS:    0,
					Error:         err.Error(),
				})
				appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, nextDecision(true, hasNext), true, lastStatusCode, 0, err)
				if hasNext {
					logger.Warn("gateway candidate skipped by provider limits",
						"provider_id", candidate.provider.ID,
						"endpoint_id", candidate.endpoint.ID,
						"credential_id", candidate.credential.ID,
						"attempt", index+1,
						"error", err,
					)
					continue
				}
				break
			}
			halfOpenProbe, err := s.runtime.acquireEndpoint(candidate, attemptStarted)
			if err != nil {
				s.runtime.releaseProvider(candidate.provider.ID, 0)
				attemptSpan.RecordError(err)
				attemptSpan.SetStatus(codes.Error, err.Error())
				attemptSpan.End()
				lastErr = err
				hasNext := index < len(candidates)-1
				attempts = append(attempts, model.RequestAttemptRecord{
					RequestID:     record.ID,
					Sequence:      index + 1,
					ProviderID:    candidate.provider.ID,
					ProviderName:  candidate.provider.Name,
					EndpointID:    candidate.endpoint.ID,
					EndpointURL:   candidate.endpoint.BaseURL,
					CredentialID:  candidate.credential.ID,
					UpstreamModel: exchange.UpstreamModel,
					StatusCode:    http.StatusServiceUnavailable,
					Retryable:     true,
					Decision:      nextDecision(true, hasNext),
					StartedAt:     attemptStarted,
					DurationMS:    0,
					Error:         err.Error(),
				})
				appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, nextDecision(true, hasNext), true, http.StatusServiceUnavailable, 0, err)
				if hasNext {
					logger.Warn("gateway candidate skipped by circuit breaker",
						"provider_id", candidate.provider.ID,
						"endpoint_id", candidate.endpoint.ID,
						"credential_id", candidate.credential.ID,
						"attempt", index+1,
						"error", err,
					)
					continue
				}
				break
			}
			closeLiveSession := s.trackRuntimeSession(LiveSessionStatus{
				RequestID:    record.ID,
				SessionID:    liveSessionID,
				GatewayKeyID: gatewayKey.ID,
				ProviderID:   candidate.provider.ID,
				ProviderName: candidate.provider.Name,
				EndpointID:   candidate.endpoint.ID,
				CredentialID: candidate.credential.ID,
				RouteAlias:   parsedRequest.routeAlias,
				Scenario:     appliedScenario,
				Protocol:     protocol,
				Transport:    "http",
				Path:         r.URL.Path,
				Stream:       parsedRequest.stream,
				StartedAt:    attemptStarted,
				LastSeenAt:   attemptStarted,
			})
			resp, err := s.doProxyRequest(attemptCtx, candidate, exchange, r.Method, r.Header, parsedRequest.rawQuery)
			if err != nil {
				closeLiveSession()
				s.runtime.releaseProvider(candidate.provider.ID, 0)
				attemptSpan.RecordError(err)
				attemptSpan.SetStatus(codes.Error, err.Error())
				attemptSpan.End()
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
				s.runtime.reportFailure(candidate, 0, err.Error(), 0, halfOpenProbe)
				if index < len(candidates)-1 {
					backoff := retryBackoffDelay(0, index+1)
					appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, "switch_provider", true, 0, backoff, err)
					logger.Warn("gateway upstream request failed before response",
						"provider_id", candidate.provider.ID,
						"endpoint_id", candidate.endpoint.ID,
						"credential_id", candidate.credential.ID,
						"attempt", index+1,
						"backoff_ms", backoff.Milliseconds(),
						"error", err,
					)
					sleepBackoff(ctx, 0, index+1)
					continue
				}
				appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, "abort", true, 0, 0, err)
				break
			}

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				closeLiveSession()
				s.runtime.releaseProvider(candidate.provider.ID, 0)
				attemptSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
				_ = resp.Body.Close()
				lastStatusCode = resp.StatusCode
				lastErr = fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
				attemptSpan.RecordError(lastErr)
				attemptSpan.SetStatus(codes.Error, lastErr.Error())
				attemptSpan.End()
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
				s.runtime.reportFailure(candidate, resp.StatusCode, strings.TrimSpace(string(body)), retryAfter, halfOpenProbe)
				if retryable && index < len(candidates)-1 {
					backoff := retryBackoffDelay(resp.StatusCode, index+1)
					appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, "switch_provider", retryable, resp.StatusCode, backoff, lastErr)
					logger.Warn("gateway upstream returned retryable error",
						"provider_id", candidate.provider.ID,
						"endpoint_id", candidate.endpoint.ID,
						"credential_id", candidate.credential.ID,
						"attempt", index+1,
						"status_code", resp.StatusCode,
						"backoff_ms", backoff.Milliseconds(),
						"error", lastErr,
					)
					sleepBackoff(ctx, resp.StatusCode, index+1)
					continue
				}
				appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, nextDecision(retryable, false), retryable, resp.StatusCode, 0, lastErr)
				break
			}

			record.AccountID = candidate.provider.ID
			record.ProviderName = candidate.provider.Name
			record.UpstreamModel = exchange.UpstreamModel
			record.StatusCode = resp.StatusCode
			record.AttemptCount = index + 1

			observer := NewUsageObserver(protocol)
			observer.ObserveRequestBody(exchange.Body)
			setBillingTrailers(w.Header())
			routingSessionID := parsedRequest.sessionID
			if routingSessionID == "" && responseTurnState != "" {
				routingSessionID = responseTurnState
			}
			writeRoutingMetadata(w.Header(), route, candidate, time.Since(attemptStarted), routingSessionID, appliedScenario)
			if responseTurnState != "" {
				w.Header().Set(headerCodexTurnState, responseTurnState)
			}
			writeErr := s.writeProxyResponse(w, resp, observer, exchange, parsedRequest.routeAlias, candidate.route.Transformers, candidate)
			_ = resp.Body.Close()
			closeLiveSession()
			if writeErr != nil {
				s.runtime.releaseProvider(candidate.provider.ID, 0)
				attemptSpan.RecordError(writeErr)
				attemptSpan.SetStatus(codes.Error, writeErr.Error())
				attemptSpan.End()
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
				s.runtime.reportFailure(candidate, 0, writeErr.Error(), 0, halfOpenProbe)
				if retryable && hasNext {
					backoff := retryBackoffDelay(0, index+1)
					appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, "switch_provider", true, http.StatusBadGateway, backoff, writeErr)
					logger.Warn("gateway response transformation failed before completion",
						"provider_id", candidate.provider.ID,
						"endpoint_id", candidate.endpoint.ID,
						"credential_id", candidate.credential.ID,
						"attempt", index+1,
						"status_code", http.StatusBadGateway,
						"backoff_ms", backoff.Milliseconds(),
						"error", writeErr,
					)
					sleepBackoff(ctx, 0, index+1)
					continue
				}
				appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, nextDecision(retryable, false), retryable, http.StatusBadGateway, 0, writeErr)
				break
			}

			record.DurationMS = time.Since(started).Milliseconds()
			record.Usage = observer.Summary()
			record.Billing = billing.Calculate(candidate.pricing, record.Usage, candidate.multiplier, candidate.pricing.Name)
			finalCost = record.Billing.FinalCost
			s.runtime.releaseProvider(candidate.provider.ID, record.Billing.FinalCost)
			attemptSpan.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
			attemptSpan.End()
			appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, index+1, "success", false, resp.StatusCode, 0, nil)
			requestSpan.SetAttributes(
				attribute.String("gateway.provider_id", candidate.provider.ID),
				attribute.String("gateway.endpoint_id", candidate.endpoint.ID),
				attribute.Int("gateway.attempt_count", index+1),
				attribute.Float64("gateway.final_cost", record.Billing.FinalCost),
			)
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
			s.runtime.reportSuccess(candidate, time.Since(attemptStarted), halfOpenProbe)
			if responseTurnState != "" && responseTurnState != parsedRequest.sessionID {
				s.runtime.bindStickyCandidate(candidate, responseTurnState, time.Now().UTC())
			}
			logger.Info("gateway request completed",
				"provider_id", candidate.provider.ID,
				"endpoint_id", candidate.endpoint.ID,
				"credential_id", candidate.credential.ID,
				"attempts", index+1,
				"status_code", resp.StatusCode,
				"duration_ms", record.DurationMS,
				"input_tokens", record.Usage.InputTokens,
				"output_tokens", record.Usage.OutputTokens,
				"total_tokens", record.Usage.TotalTokens,
				"final_cost", record.Billing.FinalCost,
			)
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
		logger.Warn("gateway request failed",
			"status_code", record.StatusCode,
			"attempts", record.AttemptCount,
			"duration_ms", record.DurationMS,
			"error", record.Error,
		)
		s.appendRecord(record, attempts)
		if lastErr != nil {
			requestSpan.RecordError(lastErr)
			requestSpan.SetStatus(codes.Error, lastErr.Error())
		}
		if responseWriteStarted(lastErr) {
			return
		}
		writeError(w, record.StatusCode, record.Error)
	}
}

func readLimitedRequestBody(body io.Reader, maxBytes int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxBytes {
		return nil, errRequestBodyTooLarge
	}
	return payload, nil
}

// ProxyRealtime proxies WebSocket traffic to OpenAI-compatible realtime APIs.
func (s *Service) ProxyRealtime(w http.ResponseWriter, r *http.Request) {
	routeAlias := strings.TrimSpace(r.URL.Query().Get("model"))
	if routeAlias == "" {
		writeError(w, http.StatusBadRequest, "model query parameter is required")
		return
	}
	sessionID := extractRealtimeSessionID(r)
	scenario := extractRealtimeScenario(r)
	ctx, requestSpan := gatewayTracer.Start(r.Context(), "gateway.proxy_realtime",
		trace.WithAttributes(
			attribute.String("gateway.protocol", string(model.ProtocolOpenAIRealtime)),
			attribute.String("http.method", r.Method),
			attribute.String("http.route", r.URL.Path),
			attribute.String("gateway.route_alias", routeAlias),
		),
	)
	defer requestSpan.End()
	if sessionID != "" {
		requestSpan.SetAttributes(attribute.String("gateway.session_id", sessionID))
	}
	if scenario != "" {
		requestSpan.SetAttributes(attribute.String("gateway.requested_scenario", scenario))
	}
	r = r.WithContext(ctx)

	state := s.store.RoutingSnapshot()
	_, authSpan := gatewayTracer.Start(ctx, "gateway.authenticate")
	gatewayKey, err := s.authenticateGatewayRequest(state, r.Header, model.ProtocolOpenAIRealtime, routeAlias, gatewayRequestSource(r))
	authSpan.End()
	if err != nil {
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
		writeGatewayAuthError(w, err)
		return
	}
	var finalCost float64
	defer func() {
		s.runtime.releaseGatewayKey(gatewayKey.ID, finalCost)
	}()

	_, planSpan := gatewayTracer.Start(ctx, "gateway.route_plan")
	candidates, route, _, appliedScenario, err := s.buildCandidatePlan(state, routeAlias, model.ProtocolOpenAIRealtime, gatewayKey.ID, sessionID, scenario)
	planSpan.SetAttributes(attribute.Int("gateway.candidate_count", len(candidates)))
	if appliedScenario != "" {
		planSpan.SetAttributes(attribute.String("gateway.scenario", appliedScenario))
	}
	planSpan.End()
	if err != nil {
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(candidates) == 0 {
		writeError(w, http.StatusBadGateway, "no realtime candidate available")
		return
	}
	candidate := candidates[0]
	if err := s.runtime.acquireProvider(candidate.provider, time.Now().UTC()); err != nil {
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
		writeError(w, providerLimitStatusCode(err), err.Error())
		return
	}
	upstreamURL, err := joinProxyURL(candidate.endpoint.BaseURL, "/v1/realtime")
	if err != nil {
		s.runtime.releaseProvider(candidate.provider.ID, 0)
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
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

	dialer, err := s.websocketDialerForProvider(candidate.provider)
	if err != nil {
		s.runtime.releaseProvider(candidate.provider.ID, 0)
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: s.allowRealtimeOrigin}
	clientConn, err := upgrader.Upgrade(w, r, routingMetadataHeader(route, candidate, 0, sessionID, appliedScenario))
	if err != nil {
		s.runtime.releaseProvider(candidate.provider.ID, 0)
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

	serverConn, _, err := dialer.DialContext(ctx, upstreamURL.String(), headers)
	if err != nil {
		s.runtime.releaseProvider(candidate.provider.ID, 0)
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
		_ = clientConn.WriteJSON(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	defer serverConn.Close()
	serverConn.SetReadLimit(maxRealtimeFrameBytes)

	started := time.Now().UTC()
	observer := NewUsageObserver(model.ProtocolOpenAIRealtime)
	record := model.RequestRecord{
		ID:              model.NewID("req"),
		Protocol:        model.ProtocolOpenAIRealtime,
		RouteAlias:      routeAlias,
		AccountID:       candidate.provider.ID,
		ProviderName:    candidate.provider.Name,
		UpstreamModel:   effectiveUpstreamModel(candidate),
		GatewayKeyID:    gatewayKey.ID,
		ClientSessionID: sessionID,
		StatusCode:      http.StatusSwitchingProtocols,
		StartedAt:       started,
		Stream:          true,
	}
	record.RoutingDecision = newRoutingDecision(route, appliedScenario, gatewayKey.ID, sessionID, candidates, s.runtime, started)
	closeLiveSession := s.trackRuntimeSession(LiveSessionStatus{
		RequestID:    record.ID,
		SessionID:    sessionID,
		GatewayKeyID: gatewayKey.ID,
		ProviderID:   candidate.provider.ID,
		ProviderName: candidate.provider.Name,
		EndpointID:   candidate.endpoint.ID,
		CredentialID: candidate.credential.ID,
		RouteAlias:   routeAlias,
		Scenario:     appliedScenario,
		Protocol:     model.ProtocolOpenAIRealtime,
		Transport:    "realtime",
		Path:         r.URL.Path,
		Stream:       true,
		StartedAt:    started,
		LastSeenAt:   started,
	})
	defer closeLiveSession()
	logger := slog.With(
		"request_id", record.ID,
		"protocol", model.ProtocolOpenAIRealtime,
		"route_alias", routeAlias,
		"gateway_key_id", gatewayKey.ID,
		"provider_id", candidate.provider.ID,
		"endpoint_id", candidate.endpoint.ID,
		"credential_id", candidate.credential.ID,
	)

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
	record.Billing = billing.Calculate(candidate.pricing, record.Usage, candidate.multiplier, candidate.pricing.Name)
	finalCost = record.Billing.FinalCost
	s.runtime.releaseProvider(candidate.provider.ID, record.Billing.FinalCost)
	requestSpan.SetAttributes(
		attribute.String("gateway.provider_id", candidate.provider.ID),
		attribute.String("gateway.endpoint_id", candidate.endpoint.ID),
		attribute.Float64("gateway.final_cost", record.Billing.FinalCost),
	)
	appendRoutingEvent(record.RoutingDecision, s.runtime, candidate, 1, "success", false, http.StatusSwitchingProtocols, 0, nil)
	if err != nil && !isNormalClose(err) {
		record.Error = err.Error()
		requestSpan.RecordError(err)
		requestSpan.SetStatus(codes.Error, err.Error())
		logger.Warn("gateway realtime session closed with error",
			"duration_ms", record.DurationMS,
			"input_tokens", record.Usage.InputTokens,
			"output_tokens", record.Usage.OutputTokens,
			"total_tokens", record.Usage.TotalTokens,
			"final_cost", record.Billing.FinalCost,
			"error", err,
		)
	} else {
		logger.Info("gateway realtime session completed",
			"duration_ms", record.DurationMS,
			"input_tokens", record.Usage.InputTokens,
			"output_tokens", record.Usage.OutputTokens,
			"total_tokens", record.Usage.TotalTokens,
			"final_cost", record.Billing.FinalCost,
		)
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

func (s *Service) doProxyRequest(ctx context.Context, candidate resolvedCandidate, exchange upstreamExchange, method string, incoming http.Header, rawQuery string) (*http.Response, error) {
	upstreamURL, err := joinProxyURL(candidate.endpoint.BaseURL, exchange.Path)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rawQuery) != "" {
		upstreamURL.RawQuery = rawQuery
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
	for _, key := range exchange.RequestHeaderRemove {
		req.Header.Del(key)
	}
	for key, values := range exchange.RequestHeaderSet {
		req.Header.Del(key)
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(exchange.Body))

	client, err := s.httpClientForProvider(candidate.provider)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := client.Do(req)
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

func (s *Service) httpClientForProvider(provider model.Provider) (*http.Client, error) {
	rawProxyURL := strings.TrimSpace(provider.ProxyURL)
	if rawProxyURL == "" {
		return s.httpClient, nil
	}

	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()

	if client := s.proxyClients[rawProxyURL]; client != nil {
		return client, nil
	}
	transport, err := transportWithProxy(s.transport, rawProxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport}
	s.proxyClients[rawProxyURL] = client
	return client, nil
}

func (s *Service) websocketDialerForProvider(provider model.Provider) (*websocket.Dialer, error) {
	rawProxyURL := strings.TrimSpace(provider.ProxyURL)
	if rawProxyURL == "" {
		return s.websocketDialer, nil
	}

	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()

	if dialer := s.proxyDialers[rawProxyURL]; dialer != nil {
		return dialer, nil
	}
	dialer, err := dialerWithProxy(s.websocketDialer, rawProxyURL)
	if err != nil {
		return nil, err
	}
	s.proxyDialers[rawProxyURL] = dialer
	return dialer, nil
}

func transportWithProxy(base *http.Transport, rawProxyURL string) (*http.Transport, error) {
	proxyURL, err := url.Parse(strings.TrimSpace(rawProxyURL))
	if err != nil {
		return nil, fmt.Errorf("invalid provider proxy url: %w", err)
	}
	transport := base.Clone()
	switch strings.ToLower(strings.TrimSpace(proxyURL.Scheme)) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)
		return transport, nil
	case "socks5", "socks5h":
		dialContext, err := proxyDialContext(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = nil
		transport.DialContext = dialContext
		return transport, nil
	default:
		return nil, fmt.Errorf("provider proxy url must use http, https, socks5, or socks5h")
	}
}

func dialerWithProxy(base *websocket.Dialer, rawProxyURL string) (*websocket.Dialer, error) {
	proxyURL, err := url.Parse(strings.TrimSpace(rawProxyURL))
	if err != nil {
		return nil, fmt.Errorf("invalid provider proxy url: %w", err)
	}
	dialer := *base
	switch strings.ToLower(strings.TrimSpace(proxyURL.Scheme)) {
	case "http", "https":
		dialer.Proxy = http.ProxyURL(proxyURL)
		return &dialer, nil
	case "socks5", "socks5h":
		dialContext, err := proxyDialContext(proxyURL)
		if err != nil {
			return nil, err
		}
		dialer.Proxy = nil
		dialer.NetDialContext = dialContext
		return &dialer, nil
	default:
		return nil, fmt.Errorf("provider proxy url must use http, https, socks5, or socks5h")
	}
}

func proxyDialContext(proxyURL *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	dialer, err := netproxy.FromURL(proxyURL, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("invalid provider proxy url: %w", err)
	}
	type contextDialer interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	}
	if contextDialer, ok := dialer.(contextDialer); ok {
		return contextDialer.DialContext, nil
	}
	return func(_ context.Context, network, address string) (net.Conn, error) {
		return dialer.Dial(network, address)
	}, nil
}

func (s *Service) appendRecord(record model.RequestRecord, attempts []model.RequestAttemptRecord) {
	if record.Billing.Currency == "" {
		record.Billing.Currency = "USD"
	}
	_ = s.store.AppendRequestRecord(record, attempts, s.cfg.RequestHistory)
}

// CircuitStatuses returns the current passive circuit state for every
// configured provider endpoint.
func (s *Service) CircuitStatuses() []EndpointCircuitStatus {
	if s == nil || s.store == nil {
		return nil
	}
	return s.runtime.CircuitStatuses(s.store.RoutingSnapshot(), time.Now().UTC())
}

// StickyBindings returns the current session-to-provider sticky routing state.
func (s *Service) StickyBindings() []StickyBindingStatus {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.StickyBindings(time.Now().UTC())
}

// ActiveSessions returns the current live runtime sessions.
func (s *Service) ActiveSessions(activeWithin time.Duration, limit int) []LiveSessionStatus {
	if s == nil || s.runtime == nil {
		return nil
	}
	return s.runtime.ActiveSessions(time.Now().UTC(), activeWithin, limit)
}

// ProviderUsage returns the current live provider runtime windows.
func (s *Service) ProviderUsage(limit int) []ProviderRuntimeStatus {
	if s == nil || s.runtime == nil || s.store == nil {
		return nil
	}
	return s.runtime.ProviderUsage(s.store.RoutingSnapshot(), time.Now().UTC(), limit)
}

// ResetCircuits clears passive circuit state for matching endpoints. Empty
// provider and endpoint filters reset every configured endpoint.
func (s *Service) ResetCircuits(providerID, endpointID string) int {
	if s == nil || s.store == nil {
		return 0
	}
	return s.runtime.ResetCircuits(s.store.RoutingSnapshot(), providerID, endpointID)
}

// ResetStickyBindings clears sticky routing state for matching sessions.
func (s *Service) ResetStickyBindings(gatewayKeyID, routeAlias, scenario, sessionID string) int {
	if s == nil || s.runtime == nil {
		return 0
	}
	return s.runtime.ResetStickyBindings(gatewayKeyID, routeAlias, scenario, sessionID, time.Now().UTC())
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

			client, err := s.httpClientForProvider(provider)
			if err != nil {
				result["status"] = "error"
				result["error"] = err.Error()
				results = append(results, result)
				continue
			}

			resp, err := client.Do(req)
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

func (s *Service) buildCandidatePlan(state model.RoutingState, alias string, protocol model.Protocol, gatewayKeyID, sessionID, scenario string) ([]resolvedCandidate, model.ModelRoute, model.PricingProfile, string, error) {
	route, ok := state.FindRoute(alias)
	if !ok {
		return nil, model.ModelRoute{}, model.PricingProfile{}, "", fmt.Errorf("route %q is not configured", alias)
	}
	route, scenario, err := applyRouteScenario(route, scenario)
	if err != nil {
		return nil, model.ModelRoute{}, model.PricingProfile{}, "", err
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
				profile, ok := state.ResolvePricingProfile(route.PricingProfileID, route.Alias, target.UpstreamModel)
				if !ok {
					profile = model.PricingProfile{
						ID:       "default",
						Name:     "default",
						Currency: "USD",
					}
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
					scenario:     scenario,
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
		return nil, model.ModelRoute{}, model.PricingProfile{}, "", fmt.Errorf("route %q has no enabled provider for protocol %s", alias, protocol)
	}

	candidates := healthy
	if len(candidates) == 0 {
		candidates = degraded
	}
	candidates = s.orderCandidatePlan(route, protocol, candidates)

	if binding, ok := s.runtime.stickyBindingFor(gatewayKeyID, stickyRouteKey(alias, scenario), sessionID, now); ok {
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
		return nil, model.ModelRoute{}, model.PricingProfile{}, "", fmt.Errorf("route %q has no candidate within provider retry budgets", alias)
	}
	return filteredCandidates, route, filteredCandidates[0].pricing, scenario, nil
}

func applyRouteScenario(route model.ModelRoute, requested string) (model.ModelRoute, string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return route, "", nil
	}
	for _, scenario := range route.Scenarios {
		if !strings.EqualFold(strings.TrimSpace(scenario.Name), requested) {
			continue
		}
		if len(scenario.Targets) == 0 {
			return route, strings.TrimSpace(scenario.Name), nil
		}
		effective := route
		effective.Targets = make([]model.RouteTarget, len(scenario.Targets))
		for index, target := range scenario.Targets {
			target.Protocols = append([]model.Protocol(nil), target.Protocols...)
			effective.Targets[index] = target
		}
		if canonical := scenario.Strategy.Canonical(); canonical != "" {
			effective.Strategy = canonical
		}
		return effective, strings.TrimSpace(scenario.Name), nil
	}
	return model.ModelRoute{}, "", fmt.Errorf("route %q does not define scenario %q", route.Alias, requested)
}

func stickyRouteKey(alias, scenario string) string {
	alias = strings.TrimSpace(alias)
	scenario = strings.TrimSpace(scenario)
	if scenario == "" {
		return alias
	}
	return alias + "\x00" + scenario
}

type candidateGroup struct {
	key          string
	targetID     string
	targetWeight int
	providerName string
	priority     int
	candidates   []resolvedCandidate
}

func (s *Service) orderCandidatePlan(route model.ModelRoute, protocol model.Protocol, candidates []resolvedCandidate) []resolvedCandidate {
	if len(candidates) <= 1 {
		return append([]resolvedCandidate(nil), candidates...)
	}

	groupIndex := make(map[string]int, len(candidates))
	groups := make([]candidateGroup, 0, len(candidates))
	for _, candidate := range candidates {
		key := candidate.target.ID + "\x00" + candidate.provider.ID
		index, ok := groupIndex[key]
		if !ok {
			index = len(groups)
			groupIndex[key] = index
			groups = append(groups, candidateGroup{
				key:          key,
				targetID:     candidate.target.ID,
				targetWeight: candidate.target.Weight,
				providerName: candidate.provider.Name,
				priority:     candidate.target.Priority,
			})
		}
		groups[index].candidates = append(groups[index].candidates, candidate)
	}

	groups = s.orderCandidateGroups(route, protocol, groups)

	ordered := make([]resolvedCandidate, 0, len(candidates))
	for _, group := range groups {
		ordered = append(ordered, s.orderProviderCandidates(route.Alias, protocol, group.key, group.candidates)...)
	}
	return ordered
}

func (s *Service) orderCandidateGroups(route model.ModelRoute, protocol model.Protocol, groups []candidateGroup) []candidateGroup {
	strategy := effectiveRouteStrategy(route)
	switch strategy {
	case model.RouteStrategyLatency:
		sort.SliceStable(groups, func(i, j int) bool {
			leftLatency := candidateGroupLatency(s.runtime, groups[i])
			rightLatency := candidateGroupLatency(s.runtime, groups[j])
			if leftLatency != rightLatency {
				return leftLatency < rightLatency
			}
			return compareCandidateGroups(groups[i], groups[j])
		})
	case model.RouteStrategyRoundRobin:
		sort.SliceStable(groups, func(i, j int) bool {
			return compareCandidateGroups(groups[i], groups[j])
		})
		offset := s.runtime.nextRoundRobinOffset(roundRobinKey(route.Alias, protocol, "route-groups"), len(groups))
		groups = rotateCandidateGroups(groups, offset)
	case model.RouteStrategyRandom:
		sort.SliceStable(groups, func(i, j int) bool {
			return compareCandidateGroups(groups[i], groups[j])
		})
		seed := time.Now().UnixNano() + int64(s.runtime.nextRoundRobinOffset(roundRobinKey(route.Alias, protocol, "route-groups-random"), len(groups)))
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(groups), func(i, j int) {
			groups[i], groups[j] = groups[j], groups[i]
		})
	case model.RouteStrategyFailover:
		sort.SliceStable(groups, func(i, j int) bool {
			if groups[i].priority != groups[j].priority {
				return groups[i].priority < groups[j].priority
			}
			if groups[i].providerName != groups[j].providerName {
				return groups[i].providerName < groups[j].providerName
			}
			return groups[i].targetID < groups[j].targetID
		})
	default:
		sort.SliceStable(groups, func(i, j int) bool {
			return compareCandidateGroups(groups[i], groups[j])
		})
	}
	return groups
}

func compareCandidateGroups(left, right candidateGroup) bool {
	if left.priority != right.priority {
		return left.priority < right.priority
	}
	if left.targetWeight != right.targetWeight {
		return left.targetWeight > right.targetWeight
	}
	if left.providerName != right.providerName {
		return left.providerName < right.providerName
	}
	return left.targetID < right.targetID
}

func candidateGroupLatency(runtime *runtimeState, group candidateGroup) int64 {
	best := int64(0)
	for index, candidate := range group.candidates {
		latency := candidateLatency(runtime.endpointLatency(candidate))
		if index == 0 || latency < best {
			best = latency
		}
	}
	if best == 0 {
		return candidateLatency(0)
	}
	return best
}

func rotateCandidateGroups(groups []candidateGroup, offset int) []candidateGroup {
	if len(groups) == 0 {
		return nil
	}
	offset %= len(groups)
	if offset == 0 {
		return groups
	}
	rotated := make([]candidateGroup, 0, len(groups))
	rotated = append(rotated, groups[offset:]...)
	rotated = append(rotated, groups[:offset]...)
	return rotated
}

func (s *Service) orderProviderCandidates(alias string, protocol model.Protocol, groupKey string, candidates []resolvedCandidate) []resolvedCandidate {
	if len(candidates) <= 1 {
		return append([]resolvedCandidate(nil), candidates...)
	}

	bands := make(map[int][]resolvedCandidate, len(candidates))
	priorities := make([]int, 0, len(candidates))
	seen := make(map[int]struct{}, len(candidates))
	for _, candidate := range candidates {
		priority := candidate.endpoint.Priority
		bands[priority] = append(bands[priority], candidate)
		if _, ok := seen[priority]; ok {
			continue
		}
		seen[priority] = struct{}{}
		priorities = append(priorities, priority)
	}
	sort.Ints(priorities)

	mode := normalizeProviderRoutingMode(candidates[0].provider.RoutingMode)
	ordered := make([]resolvedCandidate, 0, len(candidates))
	for _, priority := range priorities {
		band := append([]resolvedCandidate(nil), bands[priority]...)
		ordered = append(ordered, s.orderCandidateBand(mode, alias, protocol, groupKey, band)...)
	}
	return ordered
}

func (s *Service) orderCandidateBand(mode model.ProviderRoutingMode, alias string, protocol model.Protocol, groupKey string, band []resolvedCandidate) []resolvedCandidate {
	if len(band) <= 1 {
		return band
	}
	switch mode {
	case providerRoutingModeRoundRobin:
		offset := s.runtime.nextRoundRobinOffset(roundRobinKey(alias, protocol, groupKey), len(band))
		return rotateCandidates(band, offset)
	case providerRoutingModeRandom:
		seed := time.Now().UnixNano() + int64(s.runtime.nextRoundRobinOffset(roundRobinKey(alias, protocol, groupKey+"-random"), len(band)))
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(band), func(i, j int) {
			band[i], band[j] = band[j], band[i]
		})
		return band
	case providerRoutingModeOrdered, providerRoutingModeFailover:
		fallthrough
	default:
		sort.SliceStable(band, func(i, j int) bool {
			if band[i].endpoint.Priority != band[j].endpoint.Priority {
				return band[i].endpoint.Priority < band[j].endpoint.Priority
			}
			if band[i].endpoint.Weight != band[j].endpoint.Weight {
				return band[i].endpoint.Weight > band[j].endpoint.Weight
			}
			if band[i].endpoint.Name != band[j].endpoint.Name {
				return band[i].endpoint.Name < band[j].endpoint.Name
			}
			return band[i].endpoint.ID < band[j].endpoint.ID
		})
		return band
	}
}

func roundRobinKey(alias string, protocol model.Protocol, key string) string {
	return strings.TrimSpace(alias) + "\x00" + string(protocol) + "\x00" + strings.TrimSpace(key)
}

func rotateCandidates(candidates []resolvedCandidate, offset int) []resolvedCandidate {
	if len(candidates) == 0 {
		return nil
	}
	offset %= len(candidates)
	if offset == 0 {
		return candidates
	}
	rotated := make([]resolvedCandidate, 0, len(candidates))
	rotated = append(rotated, candidates[offset:]...)
	rotated = append(rotated, candidates[:offset]...)
	return rotated
}

func candidateLatency(latency time.Duration) int64 {
	if latency <= 0 {
		return int64(^uint(0) >> 1)
	}
	return latency.Milliseconds()
}

func effectiveRouteStrategy(route model.ModelRoute) model.RouteStrategy {
	strategy := route.Strategy.Canonical()
	if strategy == "" {
		return model.RouteStrategyPriority
	}
	return strategy
}

func routeAliasTimestamp(alias string) int64 {
	return 1735689600
}

func parseProxyRequest(protocol model.Protocol, r *http.Request, body []byte) (parsedProxyRequest, error) {
	routeAlias, stream, payload, err := parseProtocolModel(protocol, body)
	if err != nil {
		return parsedProxyRequest{}, err
	}
	request := parsedProxyRequest{
		routeAlias:  routeAlias,
		stream:      stream,
		rawBody:     body,
		rawQuery:    r.URL.RawQuery,
		jsonPayload: payload,
		sessionID:   extractRequestSessionID(payload, r.Header),
		scenario:    extractRequestScenario(payload, r.Header),
	}
	return request, nil
}

func parseProtocolModel(protocol model.Protocol, body []byte) (string, bool, map[string]any, error) {
	var payload map[string]any
	if len(bytes.TrimSpace(body)) == 0 {
		return "", false, nil, errors.New("request body is required")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false, nil, fmt.Errorf("request body must be JSON: %w", err)
	}

	switch protocol {
	case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses:
		modelName := strings.TrimSpace(readString(payload, "model"))
		if modelName == "" {
			return "", false, nil, errors.New("model is required")
		}
		return modelName, readBool(payload, "stream"), payload, nil
	case model.ProtocolAnthropic:
		modelName := strings.TrimSpace(readString(payload, "model"))
		if modelName == "" {
			return "", false, nil, errors.New("model is required")
		}
		return modelName, readBool(payload, "stream"), payload, nil
	case model.ProtocolGeminiGenerate:
		modelName := strings.TrimSpace(readString(payload, "model"))
		if modelName == "" {
			modelName = strings.TrimSpace(readString(payload, "model_name"))
		}
		if modelName == "" {
			return "", false, nil, errors.New("model is required")
		}
		return modelName, false, payload, nil
	case model.ProtocolGeminiStream:
		modelName := strings.TrimSpace(readString(payload, "model"))
		if modelName == "" {
			modelName = strings.TrimSpace(readString(payload, "model_name"))
		}
		if modelName == "" {
			return "", false, nil, errors.New("model is required")
		}
		return modelName, true, payload, nil
	default:
		return "", false, nil, fmt.Errorf("unsupported protocol %s", protocol)
	}
}

func effectiveUpstreamModel(candidate resolvedCandidate) string {
	modelName := strings.TrimSpace(candidate.target.UpstreamModel)
	if modelName == "" {
		modelName = strings.TrimSpace(candidate.route.Alias)
	}
	return modelName
}

func gatewayRequestSource(r *http.Request) string {
	if r == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil && strings.TrimSpace(host) != "" {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func routingMetadataHeader(route model.ModelRoute, candidate resolvedCandidate, latency time.Duration, sessionID, scenario string) http.Header {
	headers := http.Header{}
	writeRoutingMetadata(headers, route, candidate, latency, sessionID, scenario)
	return headers
}

func writeRoutingMetadata(headers http.Header, route model.ModelRoute, candidate resolvedCandidate, latency time.Duration, sessionID, scenario string) {
	if headers == nil {
		return
	}
	headers.Set("X-Conduit-Provider", candidate.provider.ID)
	headers.Set("X-Conduit-Route", route.Alias)
	headers.Set("X-Conduit-Endpoint", candidate.endpoint.ID)
	if sessionID != "" {
		headers.Set("X-Conduit-Session-Id", sessionID)
	}
	if scenario != "" {
		headers.Set("X-Conduit-Scenario", scenario)
	}
	if latency > 0 {
		headers.Set("X-Conduit-Latency-Ms", strconv.FormatInt(latency.Milliseconds(), 10))
	}
}

func writeBillingMetadata(headers http.Header, record model.RequestRecord) {
	if headers == nil {
		return
	}
	headers.Set("X-Gateway-Currency", record.Billing.Currency)
	headers.Set("X-Gateway-Input-Tokens", strconv.Itoa(record.Usage.InputTokens))
	headers.Set("X-Gateway-Output-Tokens", strconv.Itoa(record.Usage.OutputTokens))
	headers.Set("X-Gateway-Total-Tokens", strconv.Itoa(record.Usage.TotalTokens))
	headers.Set("X-Gateway-Billing-Input", formatBillingValue(record.Billing.InputCost))
	headers.Set("X-Gateway-Billing-Output", formatBillingValue(record.Billing.OutputCost))
	headers.Set("X-Gateway-Billing-Markup", formatBillingValue(record.Billing.Markup))
	headers.Set("X-Gateway-Billing-Final", formatBillingValue(record.Billing.FinalCost))
}

func setBillingTrailers(headers http.Header) {
	if headers == nil {
		return
	}
	trailers := []string{
		"X-Gateway-Currency",
		"X-Gateway-Input-Tokens",
		"X-Gateway-Output-Tokens",
		"X-Gateway-Total-Tokens",
		"X-Gateway-Billing-Input",
		"X-Gateway-Billing-Output",
		"X-Gateway-Billing-Markup",
		"X-Gateway-Billing-Final",
	}
	for _, key := range trailers {
		headers.Add("Trailer", key)
	}
}

func formatBillingValue(value float64) string {
	return strconv.FormatFloat(value, 'f', 6, 64)
}

func sleepBackoff(ctx context.Context, statusCode int, attempt int) {
	delay := retryBackoffDelay(statusCode, attempt)
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func retryBackoffDelay(statusCode int, attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	base := 75 * time.Millisecond
	if statusCode == http.StatusTooManyRequests || statusCode >= 500 {
		base = 150 * time.Millisecond
	}
	delay := base * time.Duration(1<<(attempt-1))
	maxDelay := 1500 * time.Millisecond
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

func providerLimitStatusCode(err error) int {
	switch {
	case errors.Is(err, errProviderRateLimit):
		return http.StatusTooManyRequests
	case errors.Is(err, errProviderConcurrencyLimit):
		return http.StatusTooManyRequests
	case errors.Is(err, errProviderHourlyBudget), errors.Is(err, errProviderDailyBudget), errors.Is(err, errProviderWeeklyBudget), errors.Is(err, errProviderMonthlyBudget):
		return http.StatusPaymentRequired
	default:
		return http.StatusBadGateway
	}
}

func copyForwardHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for key, values := range src {
		switch strings.ToLower(key) {
		case "host", "content-length", "authorization", "x-api-key", "proxy-authorization", "connection", "upgrade", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func applyHeaders(dst http.Header, headers map[string]string) {
	if dst == nil || len(headers) == 0 {
		return
	}
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		dst.Set(key, value)
	}
}

func applyProviderAuth(headers http.Header, kind model.ProviderKind, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || headers == nil {
		return
	}
	switch kind {
	case model.ProviderKindAnthropic:
		headers.Set("x-api-key", apiKey)
		headers.Set("anthropic-version", "2023-06-01")
	case model.ProviderKindGemini:
		headers.Set("x-goog-api-key", apiKey)
	default:
		headers.Set("Authorization", "Bearer "+apiKey)
	}
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
	kind := provider.Kind
	baseURL := strings.TrimSpace(endpoint.BaseURL)
	if kind == model.ProviderKindUnknown {
		switch {
		case strings.Contains(strings.ToLower(baseURL), "anthropic"):
			kind = model.ProviderKindAnthropic
		case strings.Contains(strings.ToLower(baseURL), "generativelanguage.googleapis.com"):
			kind = model.ProviderKindGemini
		default:
			kind = model.ProviderKindOpenAICompatible
		}
	}
	if capability := provider.CanonicalProtocol(); capability != "" {
		switch capability {
		case model.ProtocolAnthropic:
			kind = model.ProviderKindAnthropic
		case model.ProtocolGeminiGenerate, model.ProtocolGeminiStream:
			kind = model.ProviderKindGemini
		default:
			if kind == model.ProviderKindUnknown {
				kind = model.ProviderKindOpenAICompatible
			}
		}
	}
	switch kind {
	case model.ProviderKindAnthropic:
		return "/v1/models"
	case model.ProviderKindGemini:
		return "/v1beta/models"
	default:
		return "/v1/models"
	}
}

func (s *Service) newProbeRequest(ctx context.Context, provider model.Provider, endpoint model.ProviderEndpoint, credential model.ProviderCredential, path string) (*http.Request, error) {
	probeURL, err := joinProxyURL(endpoint.BaseURL, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL.String(), nil)
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

func joinProxyURL(base string, path string) (*url.URL, error) {
	baseURL, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return nil, fmt.Errorf("invalid base url %q: %w", base, err)
	}
	proxyPath := strings.TrimSpace(path)
	if proxyPath == "" {
		proxyPath = "/"
	}
	resolved := baseURL.ResolveReference(&url.URL{Path: proxyPath})
	return resolved, nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func proxyWSFrames(src, dst *websocket.Conn, observer *UsageObserver, errCh chan<- error) {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			errCh <- err
			return
		}
		if observer != nil && messageType == websocket.TextMessage {
			observer.ObserveJSON(payload)
		}
		if err := dst.WriteMessage(messageType, payload); err != nil {
			errCh <- err
			return
		}
	}
}

func parseRetryAfter(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		duration := time.Duration(seconds) * time.Second
		maxDuration := 15 * time.Minute
		if duration > maxDuration {
			duration = maxDuration
		}
		if duration < 0 {
			return 0
		}
		return duration
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		duration := time.Until(retryAt)
		if duration <= 0 {
			return 0
		}
		maxDuration := 15 * time.Minute
		if duration > maxDuration {
			return maxDuration
		}
		return duration
	}
	return 0
}

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func responseWriteStarted(err error) bool {
	var writeErr *proxyResponseWriteError
	return errors.As(err, &writeErr) && writeErr.started
}

type proxyResponseWriteError struct {
	err     error
	started bool
}

func (e *proxyResponseWriteError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *proxyResponseWriteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func markProxyResponseError(err error, started bool) error {
	if err == nil {
		return nil
	}
	return &proxyResponseWriteError{err: err, started: started}
}

func (s *Service) writeProxyResponse(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, exchange upstreamExchange, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	transformers = responseTransformers(transformers)
	applyResponseHeaderTransformers(w.Header(), transformers, candidate)
	if len(transformers) == 0 {
		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if exchange.Stream {
			return streamProxyResponse(w, resp, observer)
		}
		_, err := io.Copy(w, io.TeeReader(resp.Body, observer))
		return markProxyResponseError(err, true)
	}
	return s.writeTransformedResponse(w, resp, observer, exchange, publicAlias, transformers, candidate)
}

func (s *Service) writeTransformedResponse(w http.ResponseWriter, resp *http.Response, observer *UsageObserver, exchange upstreamExchange, publicAlias string, transformers []model.RouteTransformer, candidate resolvedCandidate) error {
	copyResponseHeaders(w.Header(), resp.Header)
	applyResponseHeaderTransformers(w.Header(), transformers, candidate)
	mode := exchange.ResponseMode
	switch mode {
	case responseModePassthrough:
		if exchange.Stream {
			return streamTransformedPassthrough(w, resp, observer, transformers, candidate)
		}
		payload, err := readLimitedResponseBody(resp.Body, 8<<20)
		if err != nil {
			return markProxyResponseError(err, false)
		}
		observer.ObserveResponseBody(payload)
		transformed, err := applyJSONResponseTransformers(payload, transformers, candidate)
		if err != nil {
			return markProxyResponseError(err, false)
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, err = w.Write(transformed)
		return markProxyResponseError(err, true)
	case responseModeAnthropicJSON:
		return writeAnthropicAsOpenAIJSON(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeAnthropicSSE:
		return writeAnthropicAsOpenAISSE(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeGeminiJSON:
		return writeGeminiAsOpenAIJSON(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeGeminiSSE:
		return writeGeminiAsOpenAISSE(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeResponsesJSON:
		return writeOpenAIResponsesAsJSON(w, resp, observer, publicAlias, transformers, candidate)
	case responseModeResponsesSSE:
		return writeOpenAIResponsesAsSSE(w, resp, observer, publicAlias, transformers, candidate)
	default:
		return markProxyResponseError(fmt.Errorf("unsupported response mode %s", mode), false)
	}
}

func streamProxyResponse(w http.ResponseWriter, resp *http.Response, observer *UsageObserver) error {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxStreamingScanTokenSize)
	hadEventBoundary := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(line); err != nil {
			return markProxyResponseError(err, true)
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return markProxyResponseError(err, true)
		}
		if observer != nil {
			observer.ObserveSSELine(line)
		}
		if len(line) == 0 && flusher != nil {
			hadEventBoundary = true
			flusher.Flush()
		} else if len(line) > 0 {
			hadEventBoundary = false
		}
	}
	if err := scanner.Err(); err != nil {
		return markProxyResponseError(err, true)
	}
	if flusher != nil && !hadEventBoundary {
		flusher.Flush()
	}
	return nil
}

const maxStreamingScanTokenSize = 8 << 20

func copyResponseHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-length", "connection", "transfer-encoding", "keep-alive", "proxy-connection", "upgrade", "te", "trailer":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived)
}

func (s *Service) allowRealtimeOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return false
	}
	originURL, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(originURL.Host, host)
}
