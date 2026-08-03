package inference

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
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

	mu     sync.Mutex
	coords ledger.Coords // see SetLedgerCoords
}

// TracedModelForTesting wraps m the same way NewModel wraps every provider
// kind - exported so another package's tests (replay's fixture generator)
// can drive the real duration-metric + chat-ledger-event seam with a
// scripted model.LLM, without a live provider to construct through NewModel.
func TracedModelForTesting(m model.LLM, name string) model.LLM {
	return &tracedModel{LLM: m, name: name}
}

// SetLedgerCoords stamps the coordinates every SUBSEQUENT GenerateContent
// call uses, taking precedence over whatever ctx carries. Needed because a
// worker round runs through workflow.RunNode, whose dynamic-child scheduler
// rebuilds the child's context from the context captured when the
// ENCLOSING node was scheduled - a context.WithValue done inside the
// node's own body (internal/vetting/node.go's ctx.WithAgentContext call)
// never reaches the model underneath it. This mutable field sidesteps
// that: it's the SAME Go object the agent calls directly, no ADK context
// reconstruction in between. RunGatedRefine calls this once per round; a
// model nobody calls it on just falls back to ctx (unaffected - the judge
// round, which never crosses a RunNode boundary, already worked on ctx alone).
func (t *tracedModel) SetLedgerCoords(c ledger.Coords) {
	t.mu.Lock()
	t.coords = c
	t.mu.Unlock()
}

// GenerateContent times the FULL iteration (the request streams; the
// underlying HTTP work happens as the caller ranges over it, not at this call
// itself), so the timer spans from the first pull to the iterator's exhaustion.
// It also emits one gen_ai "chat" ledger event per call (emit.go): the LAST
// response yielded stands in for "the assembled response" (a streaming
// provider's final chunk carries the accumulated turn, same assumption
// runWorkerNode's callers already make of GenerateContent's output).
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
