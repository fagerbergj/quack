package inference

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// tracedModel wraps a model.LLM to record quack.model.call.duration.
// Wrapping here (NewModel) covers every model in the system.
type tracedModel struct {
	model.LLM
	name string
	// pricing: nil = no price table entry for this model, cost metric skipped.
	pricing *config.ModelPricing

	mu     sync.Mutex
	coords ledger.Coords
}

// TracedModelForTesting wraps m like NewModel does, for tests.
func TracedModelForTesting(m model.LLM, name string) model.LLM {
	return &tracedModel{LLM: m, name: name}
}

// SetLedgerCoords stamps coordinates for subsequent GenerateContent calls,
// taking precedence over ctx (needed because RunNode rebuilds child context).
func (t *tracedModel) SetLedgerCoords(c ledger.Coords) {
	t.mu.Lock()
	t.coords = c
	t.mu.Unlock()
}

// GenerateContent times the full iteration and emits a gen_ai ledger event.
func (t *tracedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	t.mu.Lock()
	c := t.coords
	t.mu.Unlock()
	if c != (ledger.Coords{}) {
		ctx = ledger.WithCoords(ctx, c)
	}
	inner := t.LLM.GenerateContent(ctx, req, stream)
	return func(yield func(*model.LLMResponse, error) bool) {
		t0 := time.Now()
		var last *model.LLMResponse
		var callErr error
		defer func() {
			otelobs.RecordModelCallDuration(t.name, time.Since(t0))
			emitChatEvent(ctx, t.name, req, last, callErr)
			recordUsageMetrics(ctx, t.name, t.pricing, last)
		}()
		inner(func(resp *model.LLMResponse, err error) bool {
			if err != nil {
				callErr = err
			}
			if resp != nil {
				last = resp
			}
			return yield(resp, err)
		})
	}
}

// recordUsageMetrics emits gen_ai.client.token.usage (always) and
// gen_ai.client.cost (only when pricing is configured) from one completed
// call. genai's PromptTokenCount already includes cached tokens - split the
// cached subset out so the token_type series never double-count. Cost keeps
// the raw prompt total: quack has no separate cached-token price tier, so a
// cached token is billed at the input rate (see the pricing doc comment).
func recordUsageMetrics(ctx context.Context, modelName string, pricing *config.ModelPricing, resp *model.LLMResponse) {
	if resp == nil || resp.UsageMetadata == nil {
		return
	}
	u := resp.UsageMetadata
	c := ledger.CoordsFromContext(ctx)
	promptTotal := int64(u.PromptTokenCount)
	cached := int64(u.CachedContentTokenCount)
	input := promptTotal - cached
	if input < 0 {
		input = 0
	}
	output := int64(u.CandidatesTokenCount)
	reasoning := int64(u.ThoughtsTokenCount)
	otelobs.RecordTokenUsage(modelName, c.Agent, c.User, c.Source, input, output, reasoning, cached)
	if pricing != nil {
		cost := float64(promptTotal)/1e6*pricing.InputPerMTok + float64(output+reasoning)/1e6*pricing.OutputPerMTok
		otelobs.RecordCost(modelName, c.Agent, c.User, c.Source, cost)
	}
}

// Embed delegates to the wrapped model when it implements Embedder, timing the call.
func (t *tracedModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e, ok := t.LLM.(Embedder)
	if !ok {
		return nil, fmt.Errorf("inference: model %q does not implement Embed", t.name)
	}
	t0 := time.Now()
	defer func() { otelobs.RecordModelCallDuration(t.name, time.Since(t0)) }()
	return e.Embed(ctx, texts)
}
