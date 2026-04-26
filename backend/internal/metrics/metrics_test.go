package metrics

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/gateway"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

type fakeSource struct {
	uptime   float64
	counts   store.HealthCounts
	usages   []gateway.ProviderRuntimeStatus
	circuits []gateway.EndpointCircuitStatus
	active   int
	sticky   int
}

func (f fakeSource) UptimeSeconds() float64                              { return f.uptime }
func (f fakeSource) HealthCounts(_ time.Time) store.HealthCounts         { return f.counts }
func (f fakeSource) ProviderUsage(_ int) []gateway.ProviderRuntimeStatus { return f.usages }
func (f fakeSource) CircuitStatuses() []gateway.EndpointCircuitStatus    { return f.circuits }
func (f fakeSource) ActiveSessionsCount(_ time.Time) int                 { return f.active }
func (f fakeSource) StickyBindingsCount() int                            { return f.sticky }

func TestRenderEmitsExpectedMetrics(t *testing.T) {
	t.Parallel()
	src := fakeSource{
		uptime: 12.5,
		counts: store.HealthCounts{
			Providers:           3,
			Routes:              5,
			GatewayKeysTotal:    7,
			GatewayKeysActive:   6,
			Integrations:        2,
			PricingProfiles:     4,
			RequestHistoryItems: 100,
		},
		usages: []gateway.ProviderRuntimeStatus{
			{ProviderID: "p1", InFlight: 3, CurrentMinuteRequests: 12, HourlyCostUSD: 0.5, DailyCostUSD: 7.25, WeeklyCostUSD: 50, MonthlyCostUSD: 200},
		},
		circuits: []gateway.EndpointCircuitStatus{
			{ProviderID: "p1", EndpointID: "e1", State: "closed"},
			{ProviderID: "p1", EndpointID: "e2", State: "open"},
			{ProviderID: "p1", EndpointID: "e3", State: "closed"},
		},
		active: 4,
		sticky: 2,
	}
	var buf bytes.Buffer
	Render(&buf, src, time.Now().UTC())

	out := buf.String()
	for _, line := range []string{
		"conduit_uptime_seconds 12.5",
		"conduit_providers_total 3",
		"conduit_gateway_keys_active 6",
		"conduit_active_sessions 4",
		"conduit_sticky_bindings 2",
		`conduit_provider_in_flight{provider="p1"} 3`,
		`conduit_provider_minute_requests{provider="p1"} 12`,
		`conduit_provider_cost_usd{provider="p1",window="hour"} 0.5`,
		`conduit_provider_cost_usd{provider="p1",window="day"} 7.25`,
		`conduit_circuit_breakers{state="closed"} 2`,
		`conduit_circuit_breakers{state="open"} 1`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("expected metric line %q in output, got:\n%s", line, out)
		}
	}
	for _, kind := range []string{"# HELP conduit_uptime_seconds", "# TYPE conduit_uptime_seconds gauge"} {
		if !strings.Contains(out, kind) {
			t.Errorf("expected exposition header %q, got:\n%s", kind, out)
		}
	}
}

func TestHandlerRejectsNonGet(t *testing.T) {
	t.Parallel()
	h := Handler(fakeSource{})
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandlerSetsContentType(t *testing.T) {
	t.Parallel()
	h := Handler(fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("expected text/plain content-type, got %q", got)
	}
}
