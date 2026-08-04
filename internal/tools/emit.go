package tools

import (
	"context"
	"encoding/json"
	"sync"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// toolsScope names the logger every execute_tool ledger event is emitted
// through.
const toolsScope = "quack.tools"

// emitTool is the OUTERMOST wrapper on every tool this registry builds: it
// records one execute_tool ledger event per call, capturing the final
// observable behavior (including a guard denial or cancel refusal) - the
// same thing replay needs to reproduce.
//
// coords, when set, stamps node/agent (never round) and takes precedence
// over ctx: a context.WithValue stamp doesn't survive to a tool call any
// more than it does to a model call (see inference.tracedModel). It's
// mutable (SetLedgerCoords), not constructor-set, because a tool is built
// ONCE per configured agent at server startup and reused by every DAG node
// that agent runs - a node's identity isn't known until it actually runs.
type emitTool struct {
	inner runnableTool

	mu     sync.Mutex
	coords ledger.Coords
}

// emitWrap wraps t. A non-runnable tool (shouldn't happen - every tool this
// registry builds via functiontool.New is one) passes through unwrapped
// rather than failing the whole agent's tool build over missing telemetry.
func emitWrap(t tool.Tool, coords ledger.Coords) (tool.Tool, error) {
	rt, ok := t.(runnableTool)
	if !ok {
		return t, nil
	}
	return &emitTool{inner: rt, coords: coords}, nil
}

// EmitWrapForTesting applies the same execute_tool emission wrapper Build
// puts around every tool - exported so another package's tests (replay's
// fixture generator) can drive the real emission seam with a scripted tool,
// without the full Build pipeline (guards, repeat/cancel wrapping). coords,
// when non-zero, is stamped the same way Deps.LedgerCoords would be.
func EmitWrapForTesting(t tool.Tool, coords ledger.Coords) (tool.Tool, error) {
	return emitWrap(t, coords)
}

// SetLedgerCoords updates the coordinates every SUBSEQUENT call stamps, for
// a caller that only learns a node's identity when the node runs, after
// Build already built this tool. Same known ceiling as
// inference.tracedModel.SetLedgerCoords: correct for one node's own
// sequential rounds, but a shared tool used by two DIFFERENT DAG nodes
// running CONCURRENTLY can still race.
func (e *emitTool) SetLedgerCoords(c ledger.Coords) {
	e.mu.Lock()
	e.coords = c
	e.mu.Unlock()
}

func (e *emitTool) Name() string        { return e.inner.Name() }
func (e *emitTool) Description() string { return e.inner.Description() }
func (e *emitTool) IsLongRunning() bool { return e.inner.IsLongRunning() }

func (e *emitTool) Declaration() *genai.FunctionDeclaration { return e.inner.Declaration() }

// ProcessRequest packs the WRAPPER into the request's tool map, same reason
// and shape as every other wrapper in this package (see guard.go).
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
	if coords != (ledger.Coords{}) {
		emitCtx = ledger.WithCoords(ctx, coords)
	}
	emitToolEvent(emitCtx, e.Name(), args, result, err)
	return result, err
}

// emitToolEvent records one execute_tool ledger event. Marshal failures
// degrade a field to omitted, never abort emission.
func emitToolEvent(ctx context.Context, name string, args any, result map[string]any, err error) {
	if !otelobs.LoggingEnabled(toolsScope) {
		return // nothing listening - skip building a (potentially large) event nobody reads
	}
	attrs := []otellog.KeyValue{
		otellog.String(otelobs.GenAIOperationName, otelobs.GenAIOperationExecuteTool),
		otellog.String(otelobs.GenAIToolName, name),
		otellog.String(otelobs.GenAIToolType, "function"),
	}
	if b, jerr := json.Marshal(args); jerr == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIToolCallArguments, string(b)))
	}
	if b, jerr := json.Marshal(result); jerr == nil {
		attrs = append(attrs, otellog.String(otelobs.GenAIToolCallResult, string(b)))
	}
	if err != nil {
		attrs = append(attrs, otellog.String(otelobs.ErrorType, err.Error()))
	}
	// gen_ai.agent.name: same identity emitChatEvent stamps
	// (internal/inference/emit.go) - replay's stream key needs it for a
	// tool call exactly as much as it does for a chat call (both key on
	// (node, agent, round); EmitLog itself only stamps node/round/session,
	// not agent - see its doc comment).
	if c := ledger.CoordsFromContext(ctx); c.Agent != "" {
		attrs = append(attrs, otellog.String(otelobs.GenAIAgentName, c.Agent))
	}
	otelobs.EmitLog(ctx, toolsScope, "", attrs...)
}
