package inference

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference/openaimodel"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// usageEmbedder is asserted for internally so the public Embedder contract stays vectors-only.
type usageEmbedder interface {
	EmbedWithUsage(ctx context.Context, texts []string) ([][]float32, openaimodel.EmbedUsage, error)
}

// Version mirrors serve.Version (set at startup) for the llm.call payload; serve imports inference.
var Version string

// tracedModel records duration, usage and the gen_ai event for every model NewModel builds.
type tracedModel struct {
	model.LLM
	name string
	// pricing: nil = no price table entry for this model, cost metric skipped.
	pricing *config.ModelPricing
	// defaultAgent: metrics-only fallback for calls with no Coords.Agent. Never joins ctx:
	// the root chat event's Coords.Agent must stay empty.
	defaultAgent string

	mu     sync.Mutex
	coords ledger.Coords
}

// TracedModelForTesting wraps m like NewModel does, for tests.
func TracedModelForTesting(m model.LLM, name string) model.LLM {
	return &tracedModel{LLM: m, name: name}
}

// SetDefaultAgent is called once at startup, before the model serves traffic.
func (t *tracedModel) SetDefaultAgent(name string) {
	t.defaultAgent = name
}

// SetLedgerCoords stamps coords for calls whose ctx lost them (RunNode rebuilds the child ctx).
// Coords already in ctx are never overwritten.
func (t *tracedModel) SetLedgerCoords(c ledger.Coords) {
	t.mu.Lock()
	t.coords = c
	t.mu.Unlock()
}

// GenerateContent times the full iteration and emits a gen_ai ledger event.
func (t *tracedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	// The stamp fills blanks field-by-field: the run ctx already carries chat/user/source, so
	// all-or-nothing would drop node/agent/round on the worker path.
	t.mu.Lock()
	stamp := t.coords
	t.mu.Unlock()
	if !stamp.IsZero() {
		ctx = ledger.WithCoords(ctx, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), stamp))
	}
	if req != nil {
		// Past thoughts are never re-sent; dropping them here keeps the recorded input exact.
		req.Contents = openaimodel.DropThoughts(req.Contents)
	}
	setRequestSpanAttrs(ctx, req)
	inner := t.LLM.GenerateContent(ctx, req, stream)
	callCoords := ledger.CoordsFromContext(ctx)
	return func(yield func(*model.LLMResponse, error) bool) {
		t0 := time.Now()
		var last *model.LLMResponse
		var callErr error
		defer func() {
			otelobs.RecordModelCallDuration(t.name, time.Since(t0))
			emitChatEvent(ctx, t.name, req, last, callErr, t.pricing)
			recordUsageMetrics(ctx, t.name, t.defaultAgent, t.pricing, last)
			// A call cancelled by stop or shutdown says nothing about the gateway: no streak change.
			if !errors.Is(ctx.Err(), context.Canceled) {
				RecordCallResult(callCoords.ChatID, callCoords.Node, callCoords.Agent, callErr)
			}
		}()
		inner(func(resp *model.LLMResponse, err error) bool {
			if err != nil {
				callErr = err
			}
			if resp != nil {
				last = resp
				if !resp.Partial {
					// Must run before yield: ADK ends its span synchronously
					// the moment yield returns for a non-partial response.
					setResponseSpanAttrs(ctx, resp)
				}
			}
			return yield(resp, err)
		})
	}
}

// splitPromptTokens splits PromptTokenCount (which includes cached) into input/cached; every
// usage consumer shares it so the split can't drift.
func splitPromptTokens(u *genai.GenerateContentResponseUsageMetadata) (input, cached int64) {
	cached = int64(u.CachedContentTokenCount)
	input = int64(u.PromptTokenCount) - cached
	if input < 0 {
		input = 0
	}
	return input, cached
}

// recordUsageMetrics emits token usage (cached split out, so no double count) and, when priced,
// cost. defaultAgent fills the agent only when ctx carries none.
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
	input, cached := splitPromptTokens(u)
	output := int64(u.CandidatesTokenCount)
	reasoning := int64(u.ThoughtsTokenCount)
	otelobs.RecordTokenUsage(modelName, agent, c.User, c.Source, input, output, reasoning, cached)
	if pricing != nil {
		otelobs.RecordCost(modelName, agent, c.User, c.Source, callCost(pricing, int64(u.PromptTokenCount), output+reasoning))
	}
}

// callCost prices one call. No cached-token tier exists, so the whole prompt bills at the input rate.
func callCost(p *config.ModelPricing, prompt, completion int64) float64 {
	return float64(prompt)/1e6*p.InputPerMTok + float64(completion)/1e6*p.OutputPerMTok
}

// Embed shares GenerateContent's duration histogram (the embed model's name keeps its own series)
// and records input-only token usage/cost when the wrapped model reports it.
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

// recordEmbedUsage mirrors recordUsageMetrics; a usage-less response records nothing, not a zero.
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
		otelobs.RecordCost(t.name, agent, c.User, c.Source, callCost(t.pricing, u.PromptTokens, 0))
	}
}
