package inference

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// usageEmbedder is Embedder plus the token usage an OpenAI-compatible
// /embeddings response reports - an internal capability tracedModel
// type-asserts for, so the public Embedder contract stays vectors-only.
type usageEmbedder interface {
	EmbedWithUsage(ctx context.Context, texts []string) ([][]float32, openaimodel.EmbedUsage, error)
}

// tracedModel wraps a model.LLM to record quack.model.call.duration.
// Wrapping here (NewModel) covers every model in the system.
type tracedModel struct {
	model.LLM
	name string
	// pricing: nil = no price table entry for this model, cost metric skipped.
	pricing *config.ModelPricing
	// defaultAgent: metrics-only fallback agent (e.g. "orchestrator") for calls
	// with no per-round Coords.Agent. Never joins ctx - replay's StreamKey{}
	// needs the root chat event's Coords.Agent to stay empty (#617).
	defaultAgent string

	mu     sync.Mutex
	coords ledger.Coords
}

// TracedModelForTesting wraps m like NewModel does, for tests.
func TracedModelForTesting(m model.LLM, name string) model.LLM {
	return &tracedModel{LLM: m, name: name}
}

// SetDefaultAgent sets the metrics-only agent fallback (see the defaultAgent
// field doc) - called once at startup, before the model serves any traffic.
func (t *tracedModel) SetDefaultAgent(name string) {
	t.defaultAgent = name
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
			recordUsageMetrics(ctx, t.name, t.defaultAgent, t.pricing, last)
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
// defaultAgent fills the agent attribute only when ctx carries none (see
// tracedModel.defaultAgent) - never overrides a real per-round Coords.Agent.
func recordUsageMetrics(ctx context.Context, modelName, defaultAgent string, pricing *config.ModelPricing, resp *model.LLMResponse) {
	if resp == nil || resp.UsageMetadata == nil {
		return
	}
	u := resp.UsageMetadata
	c := ledger.CoordsFromContext(ctx)
	agent := c.Agent
	if agent == "" {
		agent = defaultAgent
	}
	promptTotal := int64(u.PromptTokenCount)
	cached := int64(u.CachedContentTokenCount)
	input := promptTotal - cached
	if input < 0 {
		input = 0
	}
	output := int64(u.CandidatesTokenCount)
	reasoning := int64(u.ThoughtsTokenCount)
	otelobs.RecordTokenUsage(modelName, agent, c.User, c.Source, input, output, reasoning, cached)
	if pricing != nil {
		cost := float64(promptTotal)/1e6*pricing.InputPerMTok + float64(output+reasoning)/1e6*pricing.OutputPerMTok
		otelobs.RecordCost(modelName, agent, c.User, c.Source, cost)
	}
}

// Embed delegates to the wrapped model, timing the call into the same
// quack.model.call.duration histogram GenerateContent uses (an embed call IS
// a model call, and the dashboard's latency panel already groups by the
// `model` attribute - an embed model has its own distinct name, so it lands
// in its own series there without conflating with chat-completion latency; a
// second instrument would just add a query for no separate signal). When the
// wrapped model reports token usage (openaimodel's /embeddings response),
// records gen_ai.client.token.usage/cost the same way GenerateContent does -
// embeddings have no output/reasoning/cached tokens, so only token_type=input
// is ever recorded.
func (t *tracedModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	t0 := time.Now()
	defer func() { otelobs.RecordModelCallDuration(t.name, time.Since(t0)) }()

	if ue, ok := t.LLM.(usageEmbedder); ok {
		vecs, usage, err := ue.EmbedWithUsage(ctx, texts)
		if err == nil {
			t.recordEmbedUsage(ctx, usage)
		}
		return vecs, err
	}
	e, ok := t.LLM.(Embedder)
	if !ok {
		return nil, fmt.Errorf("inference: model %q does not implement Embed", t.name)
	}
	return e.Embed(ctx, texts)
}

// recordEmbedUsage mirrors recordUsageMetrics for the embeddings shape - see
// its doc comment for the agent-attribution rule (ctx coords win, defaultAgent
// fills the gap). A zero PromptTokens (a defensive, usage-less response) records
// nothing rather than a fabricated zero.
func (t *tracedModel) recordEmbedUsage(ctx context.Context, u openaimodel.EmbedUsage) {
	if u.PromptTokens == 0 {
		return
	}
	c := ledger.CoordsFromContext(ctx)
	agent := c.Agent
	if agent == "" {
		agent = t.defaultAgent
	}
	otelobs.RecordTokenUsage(t.name, agent, c.User, c.Source, u.PromptTokens, 0, 0, 0)
	if t.pricing != nil {
		cost := float64(u.PromptTokens) / 1e6 * t.pricing.InputPerMTok
		otelobs.RecordCost(t.name, agent, c.User, c.Source, cost)
	}
}
