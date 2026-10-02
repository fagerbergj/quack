package decide

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
)

const recordScope = "quack.decide"

// record ends r's span with the policy attributes and writes the ledger
// observation; state and questions go only to the ledger, never onto the span.
func record(ctx context.Context, r Result, baseline string) {
	p := payload(r, baseline)
	if r.span != nil {
		probs, _ := json.Marshal(p.Probabilities)
		r.span.SetAttributes(
			attribute.String("quack.decision.point", p.Point), attribute.String("quack.decision.mode", p.Mode),
			attribute.String("quack.decision.handler", p.Handler), attribute.String("quack.decision.outcome", p.Outcome),
			attribute.String("quack.decision.skipped_step", r.SkippedStep()),
			attribute.Bool("quack.decision.confident", p.Confident), attribute.String("quack.decision.top", p.Top),
			attribute.Float64("quack.decision.top_p", p.TopP), attribute.String("quack.decision.baseline", p.Baseline),
			attribute.String("quack.decision.probabilities", string(probs)), attribute.Int("quack.decision.input_tokens", p.InputTokens),
			attribute.Int("quack.decision.request_bytes", p.RequestBytes), attribute.Float64("quack.decision.latency_ms", p.LatencyMS),
		)
		otelobs.End(r.span, r.Err)
	}
	if !otelobs.LoggingEnabled(recordScope) {
		return
	}
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	otelobs.EmitLog(ctx, recordScope, "",
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationDecision),
		attribute.String(otelobs.QuackDecision, string(b)))
}

func payload(r Result, baseline string) ledger.DecisionPayload {
	p := ledger.DecisionPayload{
		Point: r.Point, Mode: r.Mode, Handler: r.Handler, Outcome: string(r.Outcome), Confident: r.Confident,
		Top: r.Top, TopP: r.TopP, Probabilities: r.Answers, Baseline: baseline,
		RequestBytes: r.RequestBytes, InputTokens: r.InputTokens, ServerMS: r.ServerMS,
		LatencyMS: float64(r.Latency) / float64(time.Millisecond),
	}
	if s := r.SkippedStep(); s != "" {
		p.SkippedStep = &s
	}
	if r.Err != nil {
		p.Error = r.Err.Error()
	}
	p.State, _ = json.Marshal(r.state)
	p.Questions, _ = json.Marshal(r.questions)
	return p
}
