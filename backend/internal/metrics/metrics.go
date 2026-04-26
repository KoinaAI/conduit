// Package metrics renders a minimal Prometheus exposition document for the
// Conduit gateway. It deliberately avoids pulling in the prometheus/client_golang
// SDK; the format is well-defined and the metric set is small, so writing the
// text format directly keeps the binary lean and the moving parts auditable.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/gateway"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

// Source aggregates the runtime data sources the metrics handler needs to
// render the Prometheus document. Each method must be safe for concurrent use.
type Source interface {
	UptimeSeconds() float64
	HealthCounts(now time.Time) store.HealthCounts
	ProviderUsage(limit int) []gateway.ProviderRuntimeStatus
	CircuitStatuses() []gateway.EndpointCircuitStatus
	ActiveSessionsCount(now time.Time) int
	StickyBindingsCount() int
}

// Handler returns an http.HandlerFunc that responds to GET /metrics with a
// Prometheus text exposition. Other methods receive 405. The format follows
// the exposition spec at https://prometheus.io/docs/instrumenting/exposition_formats/.
func Handler(src Source) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		Render(w, src, time.Now().UTC())
	}
}

// Render writes the Prometheus document to w. Exposed separately so tests can
// snapshot the output without spinning up an HTTP server.
func Render(w io.Writer, src Source, now time.Time) {
	writeGauge(w, "conduit_uptime_seconds", "Process uptime in seconds.", src.UptimeSeconds(), nil)

	counts := src.HealthCounts(now)
	writeGauge(w, "conduit_providers_total", "Number of upstream providers configured.", float64(counts.Providers), nil)
	writeGauge(w, "conduit_routes_total", "Number of model route aliases configured.", float64(counts.Routes), nil)
	writeGauge(w, "conduit_gateway_keys_total", "Total gateway keys.", float64(counts.GatewayKeysTotal), nil)
	writeGauge(w, "conduit_gateway_keys_active", "Active (non-disabled) gateway keys.", float64(counts.GatewayKeysActive), nil)
	writeGauge(w, "conduit_integrations_total", "Number of upstream integrations configured.", float64(counts.Integrations), nil)
	writeGauge(w, "conduit_pricing_profiles_total", "Number of pricing profiles loaded.", float64(counts.PricingProfiles), nil)
	writeGauge(w, "conduit_request_history_items", "Items currently retained in the request history.", float64(counts.RequestHistoryItems), nil)
	writeGauge(w, "conduit_active_sessions", "Live request sessions tracked in the runtime.", float64(src.ActiveSessionsCount(now)), nil)
	writeGauge(w, "conduit_sticky_bindings", "Sticky session bindings currently held.", float64(src.StickyBindingsCount()), nil)

	usages := src.ProviderUsage(0)
	if len(usages) > 0 {
		writeHelp(w, "conduit_provider_in_flight", "In-flight requests per provider.")
		writeType(w, "conduit_provider_in_flight", "gauge")
		for _, u := range usages {
			writeSample(w, "conduit_provider_in_flight", float64(u.InFlight), labels{"provider": u.ProviderID})
		}

		writeHelp(w, "conduit_provider_minute_requests", "Requests counted in the current minute bucket per provider.")
		writeType(w, "conduit_provider_minute_requests", "gauge")
		for _, u := range usages {
			writeSample(w, "conduit_provider_minute_requests", float64(u.CurrentMinuteRequests), labels{"provider": u.ProviderID})
		}

		writeHelp(w, "conduit_provider_cost_usd", "Rolling cost per provider, by window.")
		writeType(w, "conduit_provider_cost_usd", "gauge")
		for _, u := range usages {
			writeSample(w, "conduit_provider_cost_usd", u.HourlyCostUSD, labels{"provider": u.ProviderID, "window": "hour"})
			writeSample(w, "conduit_provider_cost_usd", u.DailyCostUSD, labels{"provider": u.ProviderID, "window": "day"})
			writeSample(w, "conduit_provider_cost_usd", u.WeeklyCostUSD, labels{"provider": u.ProviderID, "window": "week"})
			writeSample(w, "conduit_provider_cost_usd", u.MonthlyCostUSD, labels{"provider": u.ProviderID, "window": "month"})
		}
	}

	circuits := src.CircuitStatuses()
	if len(circuits) > 0 {
		stateCounts := map[string]int{}
		for _, c := range circuits {
			stateCounts[strings.ToLower(strings.TrimSpace(c.State))]++
		}
		writeHelp(w, "conduit_circuit_breakers", "Endpoint circuit breakers by state.")
		writeType(w, "conduit_circuit_breakers", "gauge")
		states := make([]string, 0, len(stateCounts))
		for s := range stateCounts {
			states = append(states, s)
		}
		sort.Strings(states)
		for _, s := range states {
			label := s
			if label == "" {
				label = "unknown"
			}
			writeSample(w, "conduit_circuit_breakers", float64(stateCounts[s]), labels{"state": label})
		}
	}
}

type labels map[string]string

func writeHelp(w io.Writer, name, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(help))
}

func writeType(w io.Writer, name, kind string) {
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
}

func writeGauge(w io.Writer, name, help string, value float64, lbls labels) {
	writeHelp(w, name, help)
	writeType(w, name, "gauge")
	writeSample(w, name, value, lbls)
}

func writeSample(w io.Writer, name string, value float64, lbls labels) {
	if len(lbls) == 0 {
		fmt.Fprintf(w, "%s %s\n", name, formatFloat(value))
		return
	}
	keys := make([]string, 0, len(lbls))
	for k := range lbls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabel(lbls[k])))
	}
	fmt.Fprintf(w, "%s{%s} %s\n", name, strings.Join(parts, ","), formatFloat(value))
}

func formatFloat(v float64) string {
	// Prometheus accepts integers without a decimal point.
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func escapeHelp(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}
