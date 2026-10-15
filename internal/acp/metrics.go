package acp

import (
	sdk "github.com/coder/acp-go-sdk"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// recordUsage emits gen_ai.client.token.usage (+cost when priced) once per round from the round-aggregate Usage;
// nil emits nothing. Per-call figures derived from it are round averages; InputTokens already excludes cache reads.
func recordUsage(modelName string, coords ledger.Coords, pricing *config.ModelPricing, u *sdk.Usage) {
	if u == nil {
		return
	}
	input := int64(u.InputTokens)
	output := int64(u.OutputTokens)
	var reasoning, cached int64
	if u.ThoughtTokens != nil {
		reasoning = int64(*u.ThoughtTokens)
	}
	if u.CachedReadTokens != nil {
		cached = int64(*u.CachedReadTokens)
	}
	usage := otelobs.UsageFor(modelName, coords, "")
	otelobs.RecordTokenUsage(usage, input, output, reasoning, cached)
	if pricing != nil {
		otelobs.RecordCost(usage, callCost(pricing, input+cached, output+reasoning))
	}
}

// callCost prices prompt (cached included) and completion tokens; quack has no separate cached-token tier.
func callCost(p *config.ModelPricing, prompt, completion int64) float64 {
	return float64(prompt)/1e6*p.InputPerMTok + float64(completion)/1e6*p.OutputPerMTok
}
