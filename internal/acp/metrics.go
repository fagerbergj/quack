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
	otelobs.RecordTokenUsage(modelName, coords.Agent, coords.User, coords.Source, input, output, reasoning, cached)
	if pricing != nil {
		// cached tokens are part of the prompt too and quack has no separate
		// cached-token price tier (mirrors inference.recordUsageMetrics).
		promptTotal := input + cached
		cost := float64(promptTotal)/1e6*pricing.InputPerMTok + float64(output+reasoning)/1e6*pricing.OutputPerMTok
		otelobs.RecordCost(modelName, coords.Agent, coords.User, coords.Source, cost)
	}
}
