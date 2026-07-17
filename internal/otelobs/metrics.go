// Package otelobs is Quack's OTel wiring: tracer/meter provider setup, a
// bounded in-process ring buffer of run span trees (the zero-infra
// observability source /api/v1/obs/* and `quack obs` read from), a slog↔trace
// correlation bridge, and the metric instruments the trust gate / delivery /
// memory pipeline records against.
package otelobs

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// metrics holds every instrument Quack records against. A package-level
// singleton (mirroring OTel's own global-provider idiom): the alternative —
// threading a *Metrics handle through every gate/delivery/memory call site —
// would touch dozens of signatures across packages that don't otherwise share
// a dependency-injection seam. Nil-safe: every Record*/*Started/*Finished
// function below is a no-op until Init has run (otel.enabled: false, or
// before startup wiring completes).
var m *metrics

type metrics struct {
	runsActive   metric.Int64UpDownCounter
	nodesActive  metric.Int64UpDownCounter
	roundDur     metric.Float64Histogram // attrs: agent, model, stage
	judgeScore   metric.Float64Histogram // attrs: agent
	judgeVerdict metric.Int64Counter     // attrs: agent, passed
	delivery     metric.Int64Counter     // attrs: outcome (delivered|draft|failed|none)
	modelCallDur metric.Float64Histogram // attrs: model
	permAsk      metric.Int64Counter     // attrs: agent
	memRecall    metric.Int64Counter     // attrs: hit
}

// initMetrics builds every instrument from meter and installs it as the
// package singleton. Returns an error if any instrument fails to build (an
// OTel SDK bug, not an operator error) — callers should log and continue with
// metrics disabled rather than fail startup over an observability seam.
func initMetrics(meter metric.Meter) error {
	m2 := &metrics{}
	var err error
	if m2.runsActive, err = meter.Int64UpDownCounter("quack.runs.active",
		metric.WithDescription("orchestrator runs currently in flight")); err != nil {
		return err
	}
	if m2.nodesActive, err = meter.Int64UpDownCounter("quack.nodes.active",
		metric.WithDescription("DAG nodes currently in flight")); err != nil {
		return err
	}
	if m2.roundDur, err = meter.Float64Histogram("quack.worker.round.duration",
		metric.WithDescription("worker/judge round duration"), metric.WithUnit("s")); err != nil {
		return err
	}
	if m2.judgeScore, err = meter.Float64Histogram("quack.judge.score",
		metric.WithDescription("independent judge weakest-link score (0-1)")); err != nil {
		return err
	}
	if m2.judgeVerdict, err = meter.Int64Counter("quack.judge.verdict",
		metric.WithDescription("judge verdicts by pass/fail")); err != nil {
		return err
	}
	if m2.delivery, err = meter.Int64Counter("quack.delivery.outcome",
		metric.WithDescription("delivery outcomes; 'none' on a judge-passed work-request that recorded no delivery is the alertable phantom-success regression")); err != nil {
		return err
	}
	if m2.modelCallDur, err = meter.Float64Histogram("quack.model.call.duration",
		metric.WithDescription("model call duration, swap-sensitive"), metric.WithUnit("s")); err != nil {
		return err
	}
	if m2.permAsk, err = meter.Int64Counter("quack.acp.permission_ask",
		metric.WithDescription("ACP subprocess permission asks reaching the safety judge (should be ~0)")); err != nil {
		return err
	}
	if m2.memRecall, err = meter.Int64Counter("quack.memory.recall",
		metric.WithDescription("memory recall attempts, by hit/miss")); err != nil {
		return err
	}
	m = m2
	return nil
}

// RunStarted/RunFinished track quack.runs.active.
func RunStarted() {
	if m != nil {
		m.runsActive.Add(context.Background(), 1)
	}
}
func RunFinished() {
	if m != nil {
		m.runsActive.Add(context.Background(), -1)
	}
}

// NodeStarted/NodeFinished track quack.nodes.active.
func NodeStarted() {
	if m != nil {
		m.nodesActive.Add(context.Background(), 1)
	}
}
func NodeFinished() {
	if m != nil {
		m.nodesActive.Add(context.Background(), -1)
	}
}

// RecordRoundDuration records one worker/judge/revise round's wall time.
func RecordRoundDuration(agent, model, stage string, d time.Duration) {
	if m == nil {
		return
	}
	m.roundDur.Record(context.Background(), d.Seconds(),
		metric.WithAttributes(attrAgent(agent), attrModel(model), attrStage(stage)))
}

// RecordJudgeVerdict records one judge round's weakest-link score and pass/fail.
func RecordJudgeVerdict(agent string, score float64, passed bool) {
	if m == nil {
		return
	}
	ctx := context.Background()
	m.judgeScore.Record(ctx, score, metric.WithAttributes(attrAgent(agent)))
	m.judgeVerdict.Add(ctx, 1, metric.WithAttributes(attrAgent(agent), attrBool("passed", passed)))
}

// DeliveryOutcome names the outcome recorded by RecordDeliveryOutcome.
type DeliveryOutcome string

const (
	DeliveryDelivered DeliveryOutcome = "delivered"
	DeliveryDraft     DeliveryOutcome = "draft"
	DeliveryFailed    DeliveryOutcome = "failed"
	// DeliveryNone marks a judge-passed work-request that recorded NO
	// delivery — the phantom-success class this metric exists to catch.
	DeliveryNone DeliveryOutcome = "none"
)

// RecordDeliveryOutcome records one node's delivery outcome.
func RecordDeliveryOutcome(outcome DeliveryOutcome) {
	if m == nil {
		return
	}
	m.delivery.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", string(outcome))))
}

// RecordModelCallDuration records one model call's wall time.
func RecordModelCallDuration(model string, d time.Duration) {
	if m == nil {
		return
	}
	m.modelCallDur.Record(context.Background(), d.Seconds(), metric.WithAttributes(attrModel(model)))
}

// RecordPermissionAsk records an ACP subprocess permission ask reaching the
// safety judge — expected to stay near zero (every known ask class is
// answered in config; only novel asks reach here).
func RecordPermissionAsk(agent string) {
	if m == nil {
		return
	}
	m.permAsk.Add(context.Background(), 1, metric.WithAttributes(attrAgent(agent)))
}

// RecordMemoryRecall records one recall attempt's hit/miss.
func RecordMemoryRecall(hit bool) {
	if m == nil {
		return
	}
	m.memRecall.Add(context.Background(), 1, metric.WithAttributes(attrBool("hit", hit)))
}

func attrAgent(v string) attribute.KeyValue          { return attribute.String("agent", v) }
func attrModel(v string) attribute.KeyValue          { return attribute.String("model", v) }
func attrStage(v string) attribute.KeyValue          { return attribute.String("stage", v) }
func attrBool(key string, v bool) attribute.KeyValue { return attribute.Bool(key, v) }

// logf is a tiny startup-diagnostics helper so Init's non-fatal failures are
// still visible (metrics/tracing wiring is best-effort — never a startup error).
func logf(msg string, args ...any) {
	slog.Warn(msg, append([]any{"component", "otelobs"}, args...)...)
}
