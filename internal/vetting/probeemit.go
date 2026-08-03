package vetting

import (
	"context"
	"encoding/json"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// probeScope names the logger every gate-probe execute_tool ledger event is
// emitted through: augmentFromRepo (gitprobe.go) and checksPassCriterion's
// workspace.RunPipeline calls (checks.go) - the gate's OWN tool use on an
// external worker's behalf, deferred from #600 because neither took a
// context.Context (#604's scope addition).
const probeScope = "quack.vetting"

// probeRound is the fixed replay-ledger round these probes stamp - unlike a
// worker/judge round, a probe re-reads disk state on every activity() call
// rather than belonging to one model round, so there is no runID to reuse.
const probeRound = "gate-probe"

// emitProbeEvent records one execute_tool ledger event for a gate probe,
// same shape as tools.emitToolEvent (internal/tools/emit.go) - duplicated,
// not imported: internal/tools already depends on internal/vetting, so the
// reverse import would cycle.
func emitProbeEvent(ctx context.Context, name string, args, result any, err error) {
	if !otelobs.LoggingEnabled(probeScope) {
		return // nothing listening - skip building the event
	}
	attrs := []otellog.KeyValue{
		otellog.String(otelobs.GenAIOperationName, otelobs.GenAIOperationExecuteTool),
		otellog.String(otelobs.GenAIToolName, name),
		otellog.String(otelobs.GenAIToolType, "function"),
	}
	if args != nil {
		if b, jerr := json.Marshal(args); jerr == nil {
			attrs = append(attrs, otellog.String(otelobs.GenAIToolCallArguments, string(b)))
		}
	}
	if result != nil {
		if b, jerr := json.Marshal(result); jerr == nil {
			attrs = append(attrs, otellog.String(otelobs.GenAIToolCallResult, string(b)))
		}
	}
	if err != nil {
		attrs = append(attrs, otellog.String(otelobs.ErrorType, err.Error()))
	}
	// gen_ai.agent.name: EmitLog itself only stamps session/node/round - see
	// tools.emitToolEvent's identical comment.
	if c := ledger.CoordsFromContext(ctx); c.Agent != "" {
		attrs = append(attrs, otellog.String(otelobs.GenAIAgentName, c.Agent))
	}
	otelobs.EmitLog(ctx, probeScope, "", attrs...)
}
