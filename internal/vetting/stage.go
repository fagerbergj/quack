package vetting

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// stageSpan is the single choke point for a gate stage's lifecycle: one start/end pair raises both the
// OTel "gate.<stage>" span and the matching agent_start/agent_complete SSE events, so they can't drift.
type stageSpan struct {
	span   oteltrace.Span
	sink   func(stream.SSEEvent)
	nodeID string
}

// startStageSpan opens the span and raises agent_start. sseAgent names the SSE run's agent ("judge"); the span
// is tagged with cfg.Agent, deliberately a different field. A nil sink raises only the span.
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

// end closes the span and raises agent_complete. A nil sink (revise: SSE comes from the worker's own session events)
// raises only the span. Score/passed go on the span only for a scored judge completion, matching the SSE payload.
func (s *stageSpan) end(d stream.AgentCompleteData, err error) {
	d.FinishedAtMs = time.Now().UnixMilli()
	emitJudge(s.sink, s.nodeID, stream.SSEEvent{Name: stream.EventAgentComplete, Data: d})
	if d.Stage == stream.StageJudge && d.Status == "" {
		s.span.SetAttributes(attribute.Float64(otelobs.GenAIEvaluationScore, d.Score), attribute.Bool("verdict_passed", d.Passed))
	}
	otelobs.End(s.span, err)
}
