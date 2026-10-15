package acp

import (
	"context"

	sdk "github.com/coder/acp-go-sdk"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// recordUsage emits gen_ai.client.token.usage (+cost when priced) for one Usage: a model call's, or a round
// aggregate from an agent that reports no per-call usage. nil emits nothing; InputTokens already excludes cache reads.
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
		otelobs.RecordCost(usage, pricing.Cost(input+cached, cached, output+reasoning))
	}
}

// recordCall records one ACP model call (a usage event from translate) the way a native call is recorded:
// an llm.call on ctx's ledger stream plus token/cost metrics attributed by coords. nil u is a no-op.
func (a *Agent) recordCall(ctx context.Context, coords ledger.Coords, u *genai.GenerateContentResponseUsageMetadata) {
	if u == nil {
		return
	}
	a.emitLLMCall(ctx, u)
	cached := int(u.CachedContentTokenCount)
	recordUsage(a.opts.ModelName, coords, a.opts.Pricing, &sdk.Usage{
		InputTokens: max(int(u.PromptTokenCount)-cached, 0), OutputTokens: int(u.CandidatesTokenCount), CachedReadTokens: &cached})
}
