package tools

import (
	"context"
	"encoding/json"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// toolsScope: logger name for execute_tool ledger events.
const toolsScope = "quack.tools"

// emitTool: outermost wrapper recording one execute_tool ledger event per call.
// coords is mutable (SetLedgerCoords) because tools are built once and reused by every DAG node.
type emitTool struct {
	inner runnableTool

	mu     sync.Mutex
	coords ledger.Coords
}

// emitWrap wraps t; non-runnable tools pass through.
func emitWrap(t tool.Tool, coords ledger.Coords) (tool.Tool, error) {
	rt, ok := t.(runnableTool)
	if !ok {
		return t, nil
	}
	return &emitTool{inner: rt, coords: coords}, nil
}

// EmitWrapForTesting: same emission wrapper Build uses, exported for tests.
func EmitWrapForTesting(t tool.Tool, coords ledger.Coords) (tool.Tool, error) {
	return emitWrap(t, coords)
}

// SetLedgerCoords: updates coordinates for subsequent calls. Used when node identity is learned after Build.
func (e *emitTool) SetLedgerCoords(c ledger.Coords) {
	e.mu.Lock()
	e.coords = c
	e.mu.Unlock()
	// Forward through the wrapper chain so a guard ladder nested below (not
	// itself in StampCoords' item list) learns node identity too (#1052).
	if cs, ok := e.inner.(ledger.CoordSetter); ok {
		cs.SetLedgerCoords(c)
	}
}

func (e *emitTool) Name() string        { return e.inner.Name() }
func (e *emitTool) Description() string { return e.inner.Description() }
func (e *emitTool) IsLongRunning() bool { return e.inner.IsLongRunning() }

func (e *emitTool) Declaration() *genai.FunctionDeclaration { return e.inner.Declaration() }

// ProcessRequest packs the wrapper into the request's tool map.
func (e *emitTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := e.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[e.Name()]; ok {
			req.Tools[e.Name()] = e
		}
	}
	return nil
}

func (e *emitTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	result, err := e.inner.Run(ctx, args)
	e.mu.Lock()
	coords := e.coords
	e.mu.Unlock()
	emitCtx := context.Context(ctx)
	if !coords.IsZero() {
		emitCtx = ledger.WithCoords(ctx, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), coords))
	}
	emitToolEvent(emitCtx, e.Name(), args, result, err)
	return result, err
}

// emitToolEvent: records one execute_tool ledger event.
func emitToolEvent(ctx context.Context, name string, args any, result map[string]any, err error) {
	if !otelobs.LoggingEnabled(toolsScope) {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationExecuteTool),
		attribute.String(otelobs.GenAIToolName, name),
		attribute.String(otelobs.GenAIToolType, "function"),
	}
	if b, jerr := json.Marshal(args); jerr == nil {
		attrs = append(attrs, attribute.String(otelobs.GenAIToolCallArguments, string(b)))
	}
	if b, jerr := json.Marshal(result); jerr == nil {
		attrs = append(attrs, attribute.String(otelobs.GenAIToolCallResult, string(b)))
	}
	if err != nil {
		attrs = append(attrs, attribute.String(otelobs.ErrorType, err.Error()))
	}
	// gen_ai.agent.name: same identity emitChatEvent stamps - replay's stream key needs it.
	if c := ledger.CoordsFromContext(ctx); c.Agent != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIAgentName, c.Agent))
	}
	otelobs.EmitLog(ctx, toolsScope, "", attrs...)
}
