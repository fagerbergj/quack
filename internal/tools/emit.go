package tools

import (
	"context"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

const toolsScope = "quack.tools"

// emitTool records one execute_tool ledger event per call. coords is mutable because tools are
// built once and reused by every DAG node.
type emitTool struct {
	runnableTool

	mu     sync.Mutex
	coords ledger.Coords
}

// emitWrap wraps t; non-runnable tools pass through.
func emitWrap(t tool.Tool, coords ledger.Coords) tool.Tool {
	rt, ok := t.(runnableTool)
	if !ok {
		return t
	}
	return &emitTool{runnableTool: rt, coords: coords}
}

// rebindToolMap re-points the request's dispatch entry at the wrapper w.
func rebindToolMap(inner runnableTool, w tool.Tool, ctx agent.Context, req *model.LLMRequest) error {
	if err := inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[w.Name()]; ok {
			req.Tools[w.Name()] = w
		}
	}
	return nil
}

func (e *emitTool) SetLedgerCoords(c ledger.Coords) {
	e.mu.Lock()
	e.coords = c
	e.mu.Unlock()
	// Forward down the chain: a nested guard ladder isn't in StampCoords' item list.
	if cs, ok := e.runnableTool.(ledger.CoordSetter); ok {
		cs.SetLedgerCoords(c)
	}
}

func (e *emitTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return rebindToolMap(e.runnableTool, e, ctx, req)
}

func (e *emitTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	result, err := e.runnableTool.Run(ctx, args)
	e.mu.Lock()
	coords := e.coords
	e.mu.Unlock()
	emitCtx := context.Context(ctx)
	if !coords.IsZero() {
		emitCtx = ledger.WithCoords(ctx, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), coords))
	}
	otelobs.EmitToolCall(emitCtx, toolsScope, e.Name(), args, result, err)
	return result, err
}
