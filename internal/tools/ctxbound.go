package tools

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// ctxBoundTool: outermost wrapper - bounds every tool call to ctx even when
// the tool's own Run never checks it, so a chat-level stop always ends an
// in-flight call instead of waiting on it to return on its own.
type ctxBoundTool struct {
	inner runnableTool
}

// newCtxBoundTool wraps t; non-runnable tools pass through unbounded.
func newCtxBoundTool(t tool.Tool) tool.Tool {
	rt, ok := t.(runnableTool)
	if !ok {
		return t
	}
	return &ctxBoundTool{inner: rt}
}

func (b *ctxBoundTool) Name() string        { return b.inner.Name() }
func (b *ctxBoundTool) Description() string { return b.inner.Description() }
func (b *ctxBoundTool) IsLongRunning() bool { return b.inner.IsLongRunning() }

func (b *ctxBoundTool) SetLedgerCoords(c ledger.Coords) {
	if cs, ok := b.inner.(ledger.CoordSetter); ok {
		cs.SetLedgerCoords(c)
	}
}

func (b *ctxBoundTool) Declaration() *genai.FunctionDeclaration { return b.inner.Declaration() }

// ProcessRequest re-points dispatch at the wrapper.
func (b *ctxBoundTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := b.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[b.Name()]; ok {
			req.Tools[b.Name()] = b
		}
	}
	return nil
}

// Run races the inner call against ctx.Done(), returning ctx.Err() on cancel
// without waiting for the inner call to return.
// ponytail: the inner goroutine leaks until it finishes on its own - fine,
// the round is already tearing down. Upgrade path: a ctx-aware inner.Run.
func (b *ctxBoundTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	type result struct {
		out map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := b.inner.Run(ctx, args)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
