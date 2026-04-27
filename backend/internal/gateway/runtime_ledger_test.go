package gateway

import (
	"math"
	"testing"
	"time"
)

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestGatewaySpendLedgerTotalsExpireOnAdvance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 26, 12, 0, 0, 0, time.UTC)
	// Events must be sorted by time ascending — that's the invariant
	// production code maintains and the ledger relies on.
	ledger := newGatewaySpendLedger(now, []gatewaySpendEvent{
		{At: now.Add(-31 * 24 * time.Hour), CostUSD: 16.0}, // outside monthly (drop)
		{At: now.Add(-10 * 24 * time.Hour), CostUSD: 8.0},  // monthly only
		{At: now.Add(-2 * 24 * time.Hour), CostUSD: 4.0},   // weekly + monthly
		{At: now.Add(-2 * time.Hour), CostUSD: 1.0},        // daily + weekly + monthly
		{At: now.Add(-30 * time.Minute), CostUSD: 2.0},     // all four windows
	})

	hourly, daily, weekly, monthly := ledger.Totals(now)
	if !approxEqual(hourly, 2.0) {
		t.Fatalf("hourly: got %v want 2.0", hourly)
	}
	// daily: only events with age <= 24h => the -30m one (-2h excluded)... wait -2h > 1d? No, 2h < 24h so daily includes -2h and -30m: 1 + 2 = 3
	if !approxEqual(daily, 3.0) {
		t.Fatalf("daily: got %v want 3.0", daily)
	}
	// weekly: -2h, -30m, -2d => 1 + 2 + 4 = 7
	if !approxEqual(weekly, 7.0) {
		t.Fatalf("weekly: got %v want 7.0", weekly)
	}
	// monthly: -2h, -30m, -2d, -10d => 1 + 2 + 4 + 8 = 15. The -31d one is outside.
	if !approxEqual(monthly, 15.0) {
		t.Fatalf("monthly: got %v want 15.0", monthly)
	}
}

func TestGatewaySpendLedgerRecordIsIncremental(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.April, 26, 0, 0, 0, 0, time.UTC)
	var ledger gatewaySpendLedger

	// Record 5 events spaced 15 minutes apart, totaling 5 USD.
	for i := 0; i < 5; i++ {
		ledger.Record(start.Add(time.Duration(i)*15*time.Minute), 1.0)
	}
	now := start.Add(60 * time.Minute) // last event was 0m ago, first was 60m ago (boundary)
	hourly, daily, _, monthly := ledger.Totals(now)
	// All 5 events have age in [0, 60m], boundary inclusive at 60m: ledger uses age > Hour to expire,
	// so the event 60m ago is still inside hourly.
	if !approxEqual(hourly, 5.0) {
		t.Fatalf("hourly: got %v want 5.0", hourly)
	}
	if !approxEqual(daily, 5.0) {
		t.Fatalf("daily: got %v want 5.0", daily)
	}
	if !approxEqual(monthly, 5.0) {
		t.Fatalf("monthly: got %v want 5.0", monthly)
	}

	// Advance well past 1h since the earliest events. At T+121m, events at
	// T, T+15m, T+30m, T+45m have age > 60m and should be excluded; only
	// the event at T+60m (age = 61m... actually still > 60m). All five are
	// past, so hourly drops to 0.
	later := now.Add(61 * time.Minute) // T+121m
	hourly, daily, _, monthly = ledger.Totals(later)
	if !approxEqual(hourly, 0) {
		t.Fatalf("hourly after slide: got %v want 0", hourly)
	}
	if !approxEqual(daily, 5.0) {
		t.Fatalf("daily after slide: got %v want 5.0", daily)
	}
	if !approxEqual(monthly, 5.0) {
		t.Fatalf("monthly after slide: got %v want 5.0", monthly)
	}
}

func TestGatewaySpendLedgerCompactsMonthlyExpired(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	var ledger gatewaySpendLedger
	ledger.Record(start, 1.0)
	ledger.Record(start.Add(time.Hour), 1.0)

	// Way past 30 days.
	future := start.Add(31*24*time.Hour + time.Minute)
	hourly, daily, weekly, monthly := ledger.Totals(future)
	if !approxEqual(hourly, 0) || !approxEqual(daily, 0) || !approxEqual(weekly, 0) || !approxEqual(monthly, 0) {
		t.Fatalf("expected all windows to be empty after 30d+, got h=%v d=%v w=%v m=%v", hourly, daily, weekly, monthly)
	}
	if !ledger.IsEmpty() {
		t.Fatalf("expected ledger to be compacted, still has %d events", len(ledger.Events()))
	}
}

func TestGatewaySpendLedgerIgnoresNonPositiveCost(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.April, 26, 12, 0, 0, 0, time.UTC)
	var ledger gatewaySpendLedger
	ledger.Record(now, 0)
	ledger.Record(now, -3.0)
	if !ledger.IsEmpty() {
		t.Fatalf("expected zero/negative cost events to be ignored")
	}
}
