package vetting

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// stageSpan is the single choke point for a gate stage's lifecycle (#726): one
// startStageSpan/end pair raises both the OTel "gate.<stage>" span and the
// matching agent_start/agent_complete SSE events, so the two can't drift apart
// the way two independently hand-maintained call sites can.
type stageSpan struct {
	span   oteltrace.Span
	sink   func(stream.SSEEvent)
	nodeID string
}

// startStageSpan opens the stage's OTel span and raises agent_start. sseAgent
// names the SSE run's agent (e.g. "judge"); cfg.Agent (the node's own agent)
// is what the span is tagged with - the two are deliberately different fields.
// sink may be nil to raise only the span half (see end).
func startStageSpan(spanCtx context.Context, sink func(stream.SSEEvent), cfg Config, nodeID, sseAgent, stage, runID string, round int) (context.Context, *stageSpan) {
	ctx, span := otelobs.Start(spanCtx, "gate."+stage,
		attribute.String(otelobs.ChatIDKey, cfg.ChatID), attribute.String("node_id", nodeID),
		attribute.String("run_id", runID), attribute.String(otelobs.GenAIAgentName, cfg.Agent), attribute.Int("round", round))
	emitJudge(sink, nodeID, stream.SSEEvent{Name: stream.EventAgentStart, Data: stream.AgentStartData{
		RunID: runID, Agent: sseAgent, Stage: stage, Round: round, StartedAtMs: time.Now().UnixMilli(),
		TraceID: otelobs.TraceIDOf(ctx),
	}})
	return ctx, &stageSpan{span: span, sink: sink, nodeID: nodeID}
}

// end closes the span and raises agent_complete from the same outcome data.
// sink may be nil (revise: SSE for this run already comes from dagStream off
// the worker's own session events, so this call raises only the span half).
// Score/passed are judge-only (AgentCompleteData's own doc comment) - set on
// the span for a scored (non-error) judge completion, matching the SSE
// payload's own omission of them on a judge round that ended without a
// verdict (Status non-empty - "unavailable" or "no_verdict").
func (s *stageSpan) end(d stream.AgentCompleteData, err error) {
	emitJudge(s.sink, s.nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: d})
	if d.Stage == stream.StageJudge && d.Status == "" {
		s.span.SetAttributes(attribute.Float64(otelobs.GenAIEvaluationScore, d.Score), attribute.Bool("verdict_passed", d.Passed))
	}
	otelobs.End(s.span, err)
}
