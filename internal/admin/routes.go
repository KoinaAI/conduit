package admin

import (
	"net/http"
	"strings"
)

type RouteSpec struct {
	Method      string
	Pattern     string
	Summary     string
	HandlerName string
}

var adminRouteSpecs = []RouteSpec{
	{Method: http.MethodGet, Pattern: "/api/admin/meta", Summary: "Get admin metadata", HandlerName: "GetMeta"},
	{Method: http.MethodPost, Pattern: "/api/admin/integrations/sync", Summary: "Sync all enabled integrations", HandlerName: "SyncAllIntegrations"},
	{Method: http.MethodGet, Pattern: "/api/admin/providers", Summary: "List providers", HandlerName: "ListProviders"},
	{Method: http.MethodPost, Pattern: "/api/admin/providers", Summary: "Create provider", HandlerName: "CreateProvider"},
	{Method: http.MethodGet, Pattern: "/api/admin/providers/{id}", Summary: "Get one provider", HandlerName: "GetProvider"},
	{Method: http.MethodPut, Pattern: "/api/admin/providers/{id}", Summary: "Update one provider", HandlerName: "UpdateProvider"},
	{Method: http.MethodDelete, Pattern: "/api/admin/providers/{id}", Summary: "Delete one provider", HandlerName: "DeleteProvider"},
	{Method: http.MethodGet, Pattern: "/api/admin/routes", Summary: "List routes", HandlerName: "ListRoutes"},
	{Method: http.MethodPost, Pattern: "/api/admin/routes", Summary: "Create route", HandlerName: "CreateRoute"},
	{Method: http.MethodGet, Pattern: "/api/admin/routes/{alias}", Summary: "Get one route", HandlerName: "GetRoute"},
	{Method: http.MethodPut, Pattern: "/api/admin/routes/{alias}", Summary: "Update one route", HandlerName: "UpdateRoute"},
	{Method: http.MethodDelete, Pattern: "/api/admin/routes/{alias}", Summary: "Delete one route", HandlerName: "DeleteRoute"},
	{Method: http.MethodGet, Pattern: "/api/admin/pricing-profiles", Summary: "List pricing profiles", HandlerName: "ListPricingProfiles"},
	{Method: http.MethodPost, Pattern: "/api/admin/pricing-profiles", Summary: "Create pricing profile", HandlerName: "CreatePricingProfile"},
	{Method: http.MethodPut, Pattern: "/api/admin/pricing-profiles/{id}", Summary: "Update one pricing profile", HandlerName: "UpdatePricingProfile"},
	{Method: http.MethodDelete, Pattern: "/api/admin/pricing-profiles/{id}", Summary: "Delete one pricing profile", HandlerName: "DeletePricingProfile"},
	{Method: http.MethodGet, Pattern: "/api/admin/integrations", Summary: "List integrations", HandlerName: "ListIntegrations"},
	{Method: http.MethodPost, Pattern: "/api/admin/integrations", Summary: "Create integration", HandlerName: "CreateIntegration"},
	{Method: http.MethodPut, Pattern: "/api/admin/integrations/{id}", Summary: "Update one integration", HandlerName: "UpdateIntegration"},
	{Method: http.MethodDelete, Pattern: "/api/admin/integrations/{id}", Summary: "Delete one integration", HandlerName: "DeleteIntegration"},
	{Method: http.MethodPost, Pattern: "/api/admin/integrations/{id}/sync", Summary: "Sync one integration", HandlerName: "SyncIntegration"},
	{Method: http.MethodPost, Pattern: "/api/admin/integrations/{id}/checkin", Summary: "Run one integration daily checkin", HandlerName: "CheckinIntegration"},
	{Method: http.MethodGet, Pattern: "/api/admin/gateway-keys", Summary: "List gateway keys", HandlerName: "ListGatewayKeys"},
	{Method: http.MethodPost, Pattern: "/api/admin/gateway-keys", Summary: "Create gateway key", HandlerName: "CreateGatewayKey"},
	{Method: http.MethodPut, Pattern: "/api/admin/gateway-keys/{id}", Summary: "Update gateway key", HandlerName: "UpdateGatewayKey"},
	{Method: http.MethodDelete, Pattern: "/api/admin/gateway-keys/{id}", Summary: "Delete gateway key", HandlerName: "DeleteGatewayKey"},
	{Method: http.MethodGet, Pattern: "/api/admin/pricing-aliases", Summary: "List pricing alias rules", HandlerName: "GetPricingAliases"},
	{Method: http.MethodPut, Pattern: "/api/admin/pricing-aliases", Summary: "Replace pricing alias rules", HandlerName: "UpdatePricingAliases"},
	{Method: http.MethodGet, Pattern: "/api/admin/request-history", Summary: "List request history with filters", HandlerName: "ListRequestHistory"},
	{Method: http.MethodGet, Pattern: "/api/admin/request-history/{id}", Summary: "Get one request history record", HandlerName: "GetRequestHistoryRecord"},
	{Method: http.MethodGet, Pattern: "/api/admin/request-history/{id}/attempts", Summary: "Get recorded upstream attempts for one request", HandlerName: "GetRequestAttempts"},
	{Method: http.MethodGet, Pattern: "/api/admin/runtime/sessions", Summary: "List live runtime sessions currently tracked by the gateway", HandlerName: "ListActiveSessions"},
	{Method: http.MethodGet, Pattern: "/api/admin/runtime/sticky-bindings", Summary: "List live sticky routing bindings", HandlerName: "GetStickyBindings"},
	{Method: http.MethodPost, Pattern: "/api/admin/runtime/sticky-bindings/reset", Summary: "Reset live sticky routing bindings", HandlerName: "ResetStickyBindings"},
	{Method: http.MethodGet, Pattern: "/api/admin/runtime/provider-usage", Summary: "List live provider runtime windows currently tracked by the gateway", HandlerName: "GetProviderUsage"},
	{Method: http.MethodGet, Pattern: "/api/admin/runtime/circuits", Summary: "List passive circuit states for provider endpoints", HandlerName: "GetCircuitStatus"},
	{Method: http.MethodPost, Pattern: "/api/admin/runtime/circuits/reset", Summary: "Reset passive circuit state for matching endpoints", HandlerName: "ResetCircuits"},
	{Method: http.MethodGet, Pattern: "/api/admin/stats/summary", Summary: "Get aggregate request statistics", HandlerName: "GetStatsSummary"},
	{Method: http.MethodGet, Pattern: "/api/admin/stats/by-key", Summary: "Get request statistics grouped by gateway key", HandlerName: "GetStatsByGatewayKey"},
	{Method: http.MethodGet, Pattern: "/api/admin/stats/by-provider", Summary: "Get request statistics grouped by provider", HandlerName: "GetStatsByProvider"},
	{Method: http.MethodGet, Pattern: "/api/admin/stats/by-model", Summary: "Get request statistics grouped by routed model", HandlerName: "GetStatsByModel"},
	{Method: http.MethodGet, Pattern: "/api/admin/openapi.json", Summary: "OpenAPI document", HandlerName: "OpenAPI"},
	{Method: http.MethodPost, Pattern: "/api/admin/maintenance/checkins", Summary: "Run check-ins for all enabled integrations that support daily checkin", HandlerName: "CheckinAllIntegrations"},
	{Method: http.MethodPost, Pattern: "/api/admin/maintenance/probes", Summary: "Probe all provider endpoints", HandlerName: "ProbeAllProviders"},
	{Method: http.MethodPost, Pattern: "/api/admin/maintenance/pricing-sync", Summary: "Refresh managed pricing profiles from the public catalog", HandlerName: "SyncPricingCatalog"},
}

func RouteSpecs() []RouteSpec {
	return append([]RouteSpec(nil), adminRouteSpecs...)
}

func RegisterRoutes(mux *http.ServeMux, handlers *Handlers) {
	for _, spec := range adminRouteSpecs {
		spec := spec
		handler := adminRouteHandler(spec.HandlerName)
		mux.HandleFunc(spec.Method+" "+spec.Pattern, func(w http.ResponseWriter, r *http.Request) {
			handler(handlers, w, r)
		})
	}
}

func adminRouteHandler(name string) func(*Handlers, http.ResponseWriter, *http.Request) {
	switch name {
	case "GetMeta":
		return (*Handlers).GetMeta
	case "SyncAllIntegrations":
		return (*Handlers).SyncAllIntegrations
	case "ListProviders":
		return (*Handlers).ListProviders
	case "CreateProvider":
		return (*Handlers).CreateProvider
	case "GetProvider":
		return (*Handlers).GetProvider
	case "UpdateProvider":
		return (*Handlers).UpdateProvider
	case "DeleteProvider":
		return (*Handlers).DeleteProvider
	case "ListRoutes":
		return (*Handlers).ListRoutes
	case "CreateRoute":
		return (*Handlers).CreateRoute
	case "GetRoute":
		return (*Handlers).GetRoute
	case "UpdateRoute":
		return (*Handlers).UpdateRoute
	case "DeleteRoute":
		return (*Handlers).DeleteRoute
	case "ListPricingProfiles":
		return (*Handlers).ListPricingProfiles
	case "CreatePricingProfile":
		return (*Handlers).CreatePricingProfile
	case "UpdatePricingProfile":
		return (*Handlers).UpdatePricingProfile
	case "DeletePricingProfile":
		return (*Handlers).DeletePricingProfile
	case "ListIntegrations":
		return (*Handlers).ListIntegrations
	case "CreateIntegration":
		return (*Handlers).CreateIntegration
	case "UpdateIntegration":
		return (*Handlers).UpdateIntegration
	case "DeleteIntegration":
		return (*Handlers).DeleteIntegration
	case "SyncIntegration":
		return (*Handlers).SyncIntegration
	case "CheckinIntegration":
		return (*Handlers).CheckinIntegration
	case "ListGatewayKeys":
		return (*Handlers).ListGatewayKeys
	case "CreateGatewayKey":
		return (*Handlers).CreateGatewayKey
	case "UpdateGatewayKey":
		return (*Handlers).UpdateGatewayKey
	case "DeleteGatewayKey":
		return (*Handlers).DeleteGatewayKey
	case "GetPricingAliases":
		return (*Handlers).GetPricingAliases
	case "UpdatePricingAliases":
		return (*Handlers).UpdatePricingAliases
	case "ListRequestHistory":
		return (*Handlers).ListRequestHistory
	case "GetRequestHistoryRecord":
		return (*Handlers).GetRequestHistoryRecord
	case "GetRequestAttempts":
		return (*Handlers).GetRequestAttempts
	case "ListActiveSessions":
		return (*Handlers).ListActiveSessions
	case "GetStickyBindings":
		return (*Handlers).GetStickyBindings
	case "ResetStickyBindings":
		return (*Handlers).ResetStickyBindings
	case "GetProviderUsage":
		return (*Handlers).GetProviderUsage
	case "GetCircuitStatus":
		return (*Handlers).GetCircuitStatus
	case "ResetCircuits":
		return (*Handlers).ResetCircuits
	case "GetStatsSummary":
		return (*Handlers).GetStatsSummary
	case "GetStatsByGatewayKey":
		return (*Handlers).GetStatsByGatewayKey
	case "GetStatsByProvider":
		return (*Handlers).GetStatsByProvider
	case "GetStatsByModel":
		return (*Handlers).GetStatsByModel
	case "OpenAPI":
		return (*Handlers).OpenAPI
	case "CheckinAllIntegrations":
		return (*Handlers).CheckinAllIntegrations
	case "ProbeAllProviders":
		return (*Handlers).ProbeAllProviders
	case "SyncPricingCatalog":
		return (*Handlers).SyncPricingCatalog
	default:
		panic("unknown admin route handler: " + name)
	}
}

func buildOpenAPISpec() map[string]any {
	paths := make(map[string]any, len(adminRouteSpecs))
	for _, spec := range adminRouteSpecs {
		methods, ok := paths[spec.Pattern].(map[string]any)
		if !ok {
			methods = map[string]any{}
			paths[spec.Pattern] = methods
		}
		methods[strings.ToLower(spec.Method)] = map[string]any{
			"summary":     spec.Summary,
			"operationId": spec.HandlerName,
			"security": []map[string]any{
				{"AdminTokenHeader": []string{}},
				{"AdminBearerToken": []string{}},
			},
		}
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Conduit Admin API",
			"version":     "0.1.0",
			"description": "RESTful administrative surface for providers, routes, pricing, integrations, gateway keys, request history, and maintenance operations. This document is generated from the live admin route registry.",
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"AdminTokenHeader": map[string]any{
					"type": "apiKey",
					"in":   "header",
					"name": "X-Admin-Token",
				},
				"AdminBearerToken": map[string]any{
					"type":   "http",
					"scheme": "bearer",
				},
			},
		},
		"paths": paths,
	}
}
