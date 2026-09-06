package vetting

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// probeScope: logger for gate-probe execute_tool ledger events (augmentFromRepo, checks).
const probeScope = "quack.vetting"

// probeRound: fixed replay-ledger round for probes (no runID - re-reads disk per activity() call).
const probeRound = "gate-probe"

// emitProbeEvent: records execute_tool ledger event for a gate probe (duplicated from tools/emit.go to avoid cycle).
func emitProbeEvent(ctx context.Context, name string, args, result any, err error) {
	if !otelobs.LoggingEnabled(probeScope) {
		return // nothing listening - skip building the event
	}
	attrs := []attribute.KeyValue{
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationExecuteTool),
		attribute.String(otelobs.GenAIToolName, name),
		attribute.String(otelobs.GenAIToolType, "function"),
	}
	if args != nil {
		if b, jerr := json.Marshal(args); jerr == nil {
			attrs = append(attrs, attribute.String(otelobs.GenAIToolCallArguments, string(b)))
		}
	}
	if result != nil {
		if b, jerr := json.Marshal(result); jerr == nil {
			attrs = append(attrs, attribute.String(otelobs.GenAIToolCallResult, string(b)))
		}
	}
	if err != nil {
		attrs = append(attrs, attribute.String(otelobs.ErrorType, err.Error()))
	}
	// gen_ai.agent.name: EmitLog stamps session/node/round only.
	if c := ledger.CoordsFromContext(ctx); c.Agent != "" {
		attrs = append(attrs, attribute.String(otelobs.GenAIAgentName, c.Agent))
	}
	otelobs.EmitLog(ctx, probeScope, "", attrs...)
}
