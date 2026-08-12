package acp

import (
	sdk "github.com/coder/acp-go-sdk"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// recordUsage emits gen_ai.client.token.usage (+cost when priced) for one
// completed ACP round; u nil emits nothing, never a fabricated zero. Unlike
// genai's PromptTokenCount, opencode's InputTokens already excludes cache
// reads, so (unlike inference.recordUsageMetrics) no subtraction is needed.
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
