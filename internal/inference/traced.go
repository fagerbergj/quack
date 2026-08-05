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

// tracedModel wraps a model.LLM to record quack.model.call.duration.
// Wrapping here (NewModel) covers every model in the system.
type tracedModel struct {
	model.LLM
	name string

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
