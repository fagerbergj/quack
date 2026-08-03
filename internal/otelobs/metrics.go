// Package otelobs is Quack's OTel wiring: tracer/meter/logger provider setup
// (emission-only - Tempo/Grafana own trace/metric viewing, quack keeps no
// local store or read API of its own), a slog↔trace correlation bridge, the
// metric instruments the trust gate / delivery / memory pipeline records
// against, and EmitLog (logs.go) - the replay ledger's one emission seam.
package otelobs

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// metrics holds every instrument Quack records against. A package-level
// singleton (mirroring OTel's own global-provider idiom): the alternative -
// threading a *Metrics handle through every gate/delivery/memory call site -
// would touch dozens of signatures across packages that don't otherwise share
// a dependency-injection seam. Nil-safe: every Record*/*Started/*Finished
// function below is a no-op until Init has run (otel.enabled: false, or
// before startup wiring completes).
var m *metrics

type metrics struct {
	runsActive       metric.Int64UpDownCounter
	runsQueued       metric.Int64UpDownCounter
	nodesActive      metric.Int64UpDownCounter
	roundDur         metric.Float64Histogram // attrs: agent, model, stage (worker rounds: draft/continuation/revise/hitl/confirm)
	judgeScore       metric.Float64Histogram // attrs: agent
	judgeVerdict     metric.Int64Counter     // attrs: agent, passed
	judgeUnavailable metric.Int64Counter     // attrs: agent - judge round errored before a verdict; see RecordJudgeUnavailable
	delivery         metric.Int64Counter     // attrs: outcome (delivered|draft|failed|none)
	modelCallDur     metric.Float64Histogram // attrs: model
	permAsk          metric.Int64Counter     // attrs: agent
	memRecall        metric.Int64Counter     // attrs: hit
	checksSkipped    metric.Int64Counter     // attrs: reason - deterministic checks did NOT run at all (see RecordChecksSkipped)
	memCommitFail    metric.Int64Counter     // attrs: reason, agent - fire-and-forget memory commit that errored (see RecordMemoryCommitFailure)
	runNoAnswer      metric.Int64Counter     // a run completed (no deadline, no cancel) but persisted no final answer (see RecordRunNoAnswer)
}

// initMetrics builds every instrument from meter and installs it as the
// package singleton. Returns an error if any instrument fails to build (an
// OTel SDK bug, not an operator error) - callers should log and continue with
// metrics disabled rather than fail startup over an observability seam.
func initMetrics(meter metric.Meter) error {
	m2 := &metrics{}
	var err error
	if m2.runsActive, err = meter.Int64UpDownCounter("quack.runs.active",
		metric.WithDescription("orchestrator runs currently in flight")); err != nil {
		return err
	}
	if m2.runsQueued, err = meter.Int64UpDownCounter("quack.runs.queued",
		metric.WithDescription("orchestrator runs admitted but waiting for a concurrency slot")); err != nil {
		return err
	}
	if m2.nodesActive, err = meter.Int64UpDownCounter("quack.nodes.active",
		metric.WithDescription("DAG nodes currently in flight")); err != nil {
		return err
	}
	if m2.roundDur, err = meter.Float64Histogram("quack.worker.round.duration",
		metric.WithDescription("worker round wall time (draft/continuation/revise/hitl/confirm) - derived from the round's own span window, see StartTimedSpan"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 30, 60, 120, 300, 600)); err != nil {
		return err
	}
	if m2.judgeScore, err = meter.Float64Histogram("quack.judge.score",
		metric.WithDescription("independent judge weakest-link score (0-1); recorded on every agent's shared judge-round path (RunGatedRefine), not just one agent"),
		metric.WithExplicitBucketBoundaries(0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0)); err != nil {
		return err
	}
	if m2.judgeVerdict, err = meter.Int64Counter("quack.judge.verdict",
		metric.WithDescription("judge verdicts by pass/fail")); err != nil {
		return err
	}
	if m2.judgeUnavailable, err = meter.Int64Counter("quack.judge.unavailable",
		metric.WithDescription("judge rounds that errored before producing a verdict (no score/verdict recorded for that round) - a gap here on one agent's series explains missing/sparse quack.judge.score|verdict for it")); err != nil {
		return err
	}
	if m2.delivery, err = meter.Int64Counter("quack.delivery.outcome",
		metric.WithDescription("delivery outcomes; 'none' on a judge-passed work-request that recorded no delivery is the alertable phantom-success regression")); err != nil {
		return err
	}
	if m2.modelCallDur, err = meter.Float64Histogram("quack.model.call.duration",
		metric.WithDescription("model call duration, swap-sensitive"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 30, 60, 120, 300, 600)); err != nil {
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
	if m2.checksSkipped, err = meter.Int64Counter("quack.gate.checks.skipped",
		metric.WithDescription("nodes where the deterministic checks criterion did NOT run at all (no backstop), by reason - query this to find nodes that gated on judge score alone")); err != nil {
		return err
	}
	if m2.memCommitFail, err = meter.Int64Counter("quack.memory.commit.failures",
		metric.WithDescription("fire-and-forget memory commits that errored (consolidation/embed timeout etc - see RecordMemoryCommitFailure), by reason and agent - the only queryable signal for the M6 commit goroutine, which never fails a node")); err != nil {
		return err
	}
	if m2.runNoAnswer, err = meter.Int64Counter("quack.run.no_answer",
		metric.WithDescription("runs that finished without hitting the run deadline or being cancelled, yet persisted no final answer - the silent-gap class also covered by gate.checks.skipped/judge.unavailable/delivery.outcome=none, but at the whole-run level (see the GitHub extension's tail-comment fallback)")); err != nil {
		return err
	}
	m = m2
	return nil
}

// RunStarted/RunFinished track quack.runs.active - a run that has ACQUIRED its
// concurrency slot and is actually executing. RunQueued/RunUnqueued track
// quack.runs.queued - a run admitted (its "run" span open) but still waiting
// on max_active_runs. A run transits queued → active exactly once (or queued
// → unqueued with no active, if cancelled before it acquires); the caller
// (Orchestrator.Run) is responsible for calling exactly one matched pair from
// each, on every exit path (error, cancel, panic-unwind).
//
// Neither pair can survive a hard process kill (container restart) mid-run:
// the process that incremented the gauge never runs its decrement, and the
// counter is orphaned high until the NEXT process starts fresh at 0. That is
// inherent to an up/down counter across a restart, not a code bug - treat
// quack.runs.active/quack.runs.queued/quack.nodes.active as advisory around a
// deploy; the durable event log and Tempo traces are the source of truth for
// what was actually in flight.
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

// RunQueued/RunUnqueued track quack.runs.queued - see RunStarted's doc.
func RunQueued() {
	if m != nil {
		m.runsQueued.Add(context.Background(), 1)
	}
}
func RunUnqueued() {
	if m != nil {
		m.runsQueued.Add(context.Background(), -1)
	}
}

// NodeStarted/NodeFinished track quack.nodes.active. See RunStarted's doc for
// the restart caveat and why StartNode/EndNode are the preferred call site.
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

// StartNode opens the "node" span and marks quack.nodes.active +1 as ONE
// call. Pair with EndNode.
func StartNode(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	ctx, span := Start(ctx, "node", attrs...)
	NodeStarted()
	return ctx, span
}

// EndNode ends span (recording err) and marks quack.nodes.active -1.
func EndNode(span oteltrace.Span, err error) {
	End(span, err)
	NodeFinished()
}

// TimedSpan pairs a span with the wall-clock instant it was started, so a
// duration recorded at End is drawn from the EXACT same window the span
// itself covers - a separately-tracked t0 in the caller can drift from the
// span (e.g. code inserted between the two, or the span's own attribute
// setup taking measurable time on a slow path); this makes that impossible
// by construction.
type TimedSpan struct {
	Span  oteltrace.Span
	start time.Time
}

// StartTimedSpan opens a span, capturing its start instant for the returned
// TimedSpan.End to measure against.
func StartTimedSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, *TimedSpan) {
	ctx, span := Start(ctx, name, attrs...)
	return ctx, &TimedSpan{Span: span, start: time.Now()}
}

// End ends the span (recording err) and returns the wall-clock duration
// since StartTimedSpan - the value a caller should feed straight into a
// duration metric so it can never disagree with the span's own window.
func (ts *TimedSpan) End(err error) time.Duration {
	d := time.Since(ts.start)
	End(ts.Span, err)
	return d
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

// RecordJudgeUnavailable records a judge round that errored before it could
// produce a verdict (runJudgeAgent returned an error) - the ONE judge outcome
// RecordJudgeVerdict cannot cover, since there is no score to record. Called
// on the same shared per-agent judge path as RecordJudgeVerdict (node.go's
// judge loop) so an agent whose judge calls disproportionately error still
// gets a per-agent series instead of silently vanishing from judge.score/
// judge.verdict.
func RecordJudgeUnavailable(agent string) {
	if m == nil {
		return
	}
	m.judgeUnavailable.Add(context.Background(), 1, metric.WithAttributes(attrAgent(agent)))
}

// DeliveryOutcome names the outcome recorded by RecordDeliveryOutcome.
type DeliveryOutcome string

const (
	DeliveryDelivered DeliveryOutcome = "delivered"
	DeliveryDraft     DeliveryOutcome = "draft"
	DeliveryFailed    DeliveryOutcome = "failed"
	// DeliveryNone marks a judge-passed work-request that recorded NO
	// delivery - the phantom-success class this metric exists to catch.
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
// safety judge - expected to stay near zero (every known ask class is
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

// RecordChecksSkipped records one node whose deterministic checks criterion
// did NOT run at all - the gate then relied on judge score alone, with no
// build/vet/test backstop behind it. reason is a short machine-readable code
// (see checksPassCriterion's skip sites in internal/vetting/checks.go), e.g.
// "no_repo", "no_checks_derived", "not_configured", "no_workspace". This is
// the queryable signal for quack's phantom-success history (a fabricated
// exploration once scored 0.9; a phantom delivery shipped) - "checks passed"
// and "checks did not run" must never look the same in Tempo/Grafana.
func RecordChecksSkipped(reason string) {
	if m == nil {
		return
	}
	m.checksSkipped.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordMemoryCommitFailure records a fire-and-forget memory commit that
// errored (consolidation-model timeout, qdrant embed-write/neighbour timeout,
// or other). reason should be one of the short machine-readable buckets
// classified by the caller - see ClassifyMemoryCommitError.
func RecordMemoryCommitFailure(agent, reason string) {
	if m == nil {
		return
	}
	m.memCommitFail.Add(context.Background(), 1, metric.WithAttributes(attrAgent(agent), attribute.String("reason", reason)))
}

// ClassifyMemoryCommitError buckets a memory-commit error into a short
// machine-readable reason for RecordMemoryCommitFailure's "reason" attribute,
// so Grafana can distinguish the consolidation-model timeout from the
// embedder timeout instead of lumping every failure into one series.
func ClassifyMemoryCommitError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "consolidat"):
		return "consolidation"
	case strings.Contains(msg, "neighbour") || strings.Contains(msg, "neighbor"):
		return "embed_neighbours"
	case strings.Contains(msg, "embed"):
		return "embed_writes"
	default:
		return "other"
	}
}

// RecordRunNoAnswer records a run that ran to completion (not deadline-killed,
// not cancelled) but persisted no final answer text - see webhook.go's tail
// comment, which posts a loud failure instead of a silent placeholder for
// exactly this case.
func RecordRunNoAnswer() {
	if m == nil {
		return
	}
	m.runNoAnswer.Add(context.Background(), 1)
}

func attrAgent(v string) attribute.KeyValue          { return attribute.String("agent", v) }
func attrModel(v string) attribute.KeyValue          { return attribute.String("model", v) }
func attrStage(v string) attribute.KeyValue          { return attribute.String("stage", v) }
func attrBool(key string, v bool) attribute.KeyValue { return attribute.Bool(key, v) }

// logf is a tiny startup-diagnostics helper so Init's non-fatal failures are
// still visible (metrics/tracing wiring is best-effort - never a startup error).
func logf(msg string, args ...any) {
	slog.Warn(msg, append([]any{"component", "otelobs"}, args...)...)
}
