package tools

import (
	"encoding/json"

	otellog "go.opentelemetry.io/otel/log"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// toolsScope names the logger every execute_tool ledger event is emitted
// through.
const toolsScope = "quack.tools"

// emitTool is the OUTERMOST wrapper on every tool this registry builds: it
// records one execute_tool ledger event per call, capturing what actually
// ran (including a guard denial or cancel refusal - final observable
// behavior, the same thing replay needs to reproduce). node/round/agent
// come off ctx (ledger.CoordsFromContext, via otelobs.EmitLog) - the calling
// worker round already stamped them, so this wrapper needs no identity of
// its own.
type emitTool struct {
	inner runnableTool
}

// emitWrap wraps t. A non-runnable tool (shouldn't happen - every tool this
// registry builds via functiontool.New is one) passes through unwrapped
// rather than failing the whole agent's tool build over missing telemetry.
func emitWrap(t tool.Tool) (tool.Tool, error) {
	rt, ok := t.(runnableTool)
	if !ok {
		return t, nil
	}
	return &emitTool{inner: rt}, nil
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
	emitToolEvent(ctx, e.Name(), args, result, err)
	return result, err
}

// emitToolEvent records one execute_tool ledger event. Marshal failures
// degrade a field to omitted, never abort emission.
func emitToolEvent(ctx agent.Context, name string, args any, result map[string]any, err error) {
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
	otelobs.EmitLog(ctx, toolsScope, "", attrs...)
}
