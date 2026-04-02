package billing

import (
	"math"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

func Calculate(profile model.PricingProfile, usage model.UsageSummary, multiplier float64, source string) model.BillingSummary {
	if multiplier <= 0 {
		multiplier = 1
	}

	baseCost := profile.RequestFlat
	baseCost += costForMillion(profile.InputPerMillion, usage.InputTokens)
	baseCost += costForMillion(profile.OutputPerMillion, usage.OutputTokens)
	baseCost += costForMillion(profile.CachedInputPerMillion, usage.CachedInputTokens)
	baseCost += costForMillion(profile.ReasoningPerMillion, usage.ReasoningTokens)

	return model.BillingSummary{
		Currency:   profile.Currency,
		BaseCost:   round(baseCost),
		FinalCost:  round(baseCost * multiplier),
		Multiplier: multiplier,
		Source:     source,
	}
}

func costForMillion(unit float64, tokens int64) float64 {
	if unit <= 0 || tokens <= 0 {
		return 0
	}
	return unit * float64(tokens) / 1_000_000
}

func round(value float64) float64 {
	const scale = 1_000_000
	if value == 0 {
		return 0
	}
	return math.Round(value*scale) / scale
}
