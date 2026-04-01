package billing

import (
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestCalculate(t *testing.T) {
	t.Parallel()

	profile := model.PricingProfile{
		ID:                    "p1",
		Name:                  "standard",
		Currency:              "USD",
		InputPerMillion:       1.2,
		OutputPerMillion:      2.4,
		CachedInputPerMillion: 0.3,
		ReasoningPerMillion:   0.8,
		RequestFlat:           0.01,
	}

	usage := model.UsageSummary{
		InputTokens:       10_000,
		OutputTokens:      5_000,
		CachedInputTokens: 2_000,
		ReasoningTokens:   1_000,
	}

	result := Calculate(profile, usage, 1.5, profile.Name)

	if result.Currency != "USD" {
		t.Fatalf("unexpected currency: %s", result.Currency)
	}
	if result.BaseCost != 0.0354 {
		t.Fatalf("unexpected base cost: %.4f", result.BaseCost)
	}
	if result.FinalCost != 0.0531 {
		t.Fatalf("unexpected final cost: %.4f", result.FinalCost)
	}
}
