package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func newRoutingDecision(route model.ModelRoute, scenario, gatewayKeyID, sessionID string, candidates []resolvedCandidate, runtime *runtimeState, now time.Time) *model.RoutingDecision {
	if len(candidates) == 0 {
		return nil
	}
	trace := &model.RoutingDecision{
		Strategy:  effectiveRouteStrategy(route),
		Scenario:  strings.TrimSpace(scenario),
		SessionID: sessionID,
		Events:    []model.RoutingDecisionEvent{},
	}
	trace.Candidates = make([]model.RoutingCandidate, 0, len(candidates))
	stickyBinding, hasSticky := runtime.stickyBindingFor(gatewayKeyID, stickyRouteKey(route.Alias, scenario), sessionID, now)
	for _, candidate := range candidates {
		sticky := hasSticky &&
			candidate.provider.ID == stickyBinding.ProviderID &&
			candidate.endpoint.ID == stickyBinding.EndpointID &&
			candidate.credential.ID == stickyBinding.CredentialID
		trace.Candidates = append(trace.Candidates, routingCandidate(candidate, runtime, now, sticky))
	}
	return trace
}

func routingCandidate(candidate resolvedCandidate, runtime *runtimeState, now time.Time, sticky bool) model.RoutingCandidate {
	return model.RoutingCandidate{
		ProviderID:    candidate.provider.ID,
		ProviderName:  candidate.provider.Name,
		EndpointID:    candidate.endpoint.ID,
		CredentialID:  candidate.credential.ID,
		UpstreamModel: effectiveUpstreamModel(candidate),
		Priority:      candidate.target.Priority + candidate.endpoint.Priority,
		Weight:        totalCandidateWeight(candidate),
		LatencyMS:     runtime.endpointLatency(candidate),
		Healthy:       !runtime.endpointOpen(candidate, now) && !runtime.credentialCoolingDown(candidate, now),
		Sticky:        sticky,
	}
}

func appendRoutingEvent(trace *model.RoutingDecision, runtime *runtimeState, candidate resolvedCandidate, attempt int, decision string, retryable bool, statusCode int, backoff time.Duration, err error) {
	if trace == nil {
		return
	}
	event := model.RoutingDecisionEvent{
		Attempt:      attempt,
		ProviderID:   candidate.provider.ID,
		EndpointID:   candidate.endpoint.ID,
		CredentialID: candidate.credential.ID,
		Decision:     decision,
		StatusCode:   statusCode,
		Retryable:    retryable,
		BackoffMS:    backoff.Milliseconds(),
	}
	if err != nil {
		event.Error = err.Error()
	}
	trace.Events = append(trace.Events, event)
	if decision == "success" {
		trace.Selected = selectedRoutingCandidate(trace, candidate)
	}
}

func writeRoutingMetadata(headers http.Header, route model.ModelRoute, candidate resolvedCandidate, latency time.Duration, sessionID, scenario string) {
	headers.Set("X-Conduit-Route", route.Alias)
	headers.Set("X-Conduit-Strategy", string(effectiveRouteStrategy(route)))
	headers.Set("X-Conduit-Provider", candidate.provider.ID)
	headers.Set("X-Conduit-Endpoint", candidate.endpoint.ID)
	headers.Set("X-Conduit-Upstream-Model", effectiveUpstreamModel(candidate))
	headers.Set("X-Conduit-Latency-Ms", strconv.FormatInt(latency.Milliseconds(), 10))
	if sessionID != "" {
		headers.Set("X-Conduit-Session-Id", sessionID)
	}
	if strings.TrimSpace(scenario) != "" {
		headers.Set("X-Conduit-Scenario", strings.TrimSpace(scenario))
	}
}

func routingMetadataHeader(route model.ModelRoute, candidate resolvedCandidate, latency time.Duration, sessionID, scenario string) http.Header {
	headers := http.Header{}
	writeRoutingMetadata(headers, route, candidate, latency, sessionID, scenario)
	return headers
}

func selectedRoutingCandidate(trace *model.RoutingDecision, candidate resolvedCandidate) *model.RoutingCandidate {
	for index := range trace.Candidates {
		recorded := trace.Candidates[index]
		if recorded.ProviderID != candidate.provider.ID {
			continue
		}
		if recorded.EndpointID != candidate.endpoint.ID {
			continue
		}
		if recorded.CredentialID != candidate.credential.ID {
			continue
		}
		selected := recorded
		selected.Sticky = false
		return &selected
	}

	selected := model.RoutingCandidate{
		ProviderID:    candidate.provider.ID,
		ProviderName:  candidate.provider.Name,
		EndpointID:    candidate.endpoint.ID,
		CredentialID:  candidate.credential.ID,
		UpstreamModel: effectiveUpstreamModel(candidate),
		Priority:      candidate.target.Priority + candidate.endpoint.Priority,
		Weight:        totalCandidateWeight(candidate),
	}
	return &selected
}
