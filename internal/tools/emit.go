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
// records one execute_tool ledger event per call, capturing what actually
// ran (including a guard denial or cancel refusal - final observable
// behavior, the same thing replay needs to reproduce).
//
// coords, when set, stamps node/agent (never round - a tool is built once
// per agent, before any round exists, and reused across every round of
// it), taking precedence over ctx: the same context-reconstruction issue
// inference.tracedModel.SetLedgerCoords works around for the model call
// also loses a context.WithValue stamp before it reaches a tool call. Zero
// value falls back to ctx (ledger.CoordsFromContext, via otelobs.EmitLog).
// Mutable (SetLedgerCoords), not just constructor-set: a builtin tool is
// built ONCE per configured agent (tools.Build, at server startup) and
// reused by every DAG node that agent runs - a node's identity isn't known
// until it actually runs, long after the tool exists.
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

// SetLedgerCoords updates the coordinates every SUBSEQUENT call stamps -
// implements ledger.CoordSetter, the runtime twin of Deps.LedgerCoords/the
// emitWrap constructor argument, for a caller (dag.buildGateNodes, via
// ledger.StampCoords) that only learns a node's identity when the node
// actually runs, after Build already built this tool. Same mechanism and
// the same known ceiling as inference.tracedModel.SetLedgerCoords: correct
// for one node's own sequential rounds; a shared tool instance used by two
// DIFFERENT DAG nodes running CONCURRENTLY (same agent, parallel plan
// nodes, or two chats at once) can still race - see that method's doc
// comment.
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
