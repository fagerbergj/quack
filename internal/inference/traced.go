package inference

import (
	"context"
	"fmt"
	"iter"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// tracedModel wraps a model.LLM to record quack.model.call.duration - the
// swap-sensitive metric (a shared local server can only have one model
// resident at a time, so a slow call is often a SWAP, not just a slow
// generation). Wrapping here, the single factory (NewModel), means every
// model in the system - worker, judge, orchestrator, titler, compaction
// fallback, embedder/consolidation - is covered for free, with no call site
// changes anywhere else.
type tracedModel struct {
	model.LLM
	name string
}

// GenerateContent times the FULL iteration (the request streams; the
// underlying HTTP work happens as the caller ranges over it, not at this call
// itself), so the timer spans from the first pull to the iterator's exhaustion.
func (t *tracedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	inner := t.LLM.GenerateContent(ctx, req, stream)
	return func(yield func(*model.LLMResponse, error) bool) {
		t0 := time.Now()
		defer func() { otelobs.RecordModelCallDuration(t.name, time.Since(t0)) }()
		inner(yield)
	}
}

// Embed delegates to the wrapped model when it implements Embedder (every
// kind implemented today does) - keeps tracedModel usable as the Embedder
// NewEmbedder's type assertion expects, while still timing the call.
func (t *tracedModel) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e, ok := t.LLM.(Embedder)
	if !ok {
		return nil, fmt.Errorf("inference: model %q does not implement Embed", t.name)
	}
	t0 := time.Now()
	defer func() { otelobs.RecordModelCallDuration(t.name, time.Since(t0)) }()
	return e.Embed(ctx, texts)
}
