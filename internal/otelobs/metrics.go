// Package otelobs is Quack's OTel wiring: provider setup, metrics, and the ledger's observation emission seam.
package otelobs

import (
	"cmp"
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/ledger"
)

// package-level singleton; nil-safe until Init runs.
var m *metrics

// InitMetricsForTesting installs meter's instruments as the singleton, so another package's test can
// record against a real metric.ManualReader.
func InitMetricsForTesting(meter metric.Meter) error {
	return initMetrics(meter)
}

// MetricPoint is one collected data point: its attributes as strings and its value (a histogram's count).
type MetricPoint struct {
	Attrs map[string]string
	Value float64
}

// PointsForTesting collects reader and returns every data point of the instrument name, for tests that
// assert on labels; an unrecorded instrument returns none.
func PointsForTesting(reader *sdkmetric.ManualReader, name string) ([]MetricPoint, error) {
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		return nil, err
	}
	var out []MetricPoint
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name == name {
				out = append(out, metricPoints(met.Data)...)
			}
		}
	}
	return out, nil
}

func metricPoints(data metricdata.Aggregation) []MetricPoint {
	var out []MetricPoint
	add := func(set attribute.Set, v float64) {
		attrs := map[string]string{}
		for _, kv := range set.ToSlice() {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		out = append(out, MetricPoint{Attrs: attrs, Value: v})
	}
	switch d := data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes, float64(dp.Value))
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes, dp.Value)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			add(dp.Attributes, float64(dp.Count))
		}
	}
	return out
}

type metrics struct {
	runsActive       metric.Int64UpDownCounter
	nodesActive      metric.Int64UpDownCounter
	roundDur         metric.Float64Histogram // attrs: agent, model, stage
	judgeScore       metric.Float64Histogram // attrs: agent
	judgeVerdict     metric.Int64Counter     // attrs: agent, passed, first_round
	judgeUnavailable metric.Int64Counter     // attrs: agent
	judgePhase       metric.Float64Histogram // attrs: agent, phase, criterion (verify only)
	delivery         metric.Int64Counter     // attrs: outcome
	modelCallDur     metric.Float64Histogram // attrs: model
	permAsk          metric.Int64Counter     // attrs: agent
	memRecall        metric.Int64Counter     // attrs: hit
	checksSkipped    metric.Int64Counter     // attrs: reason
	memCommitFail    metric.Int64Counter     // attrs: reason, agent
	runNoAnswer      metric.Int64Counter
	tokenUsage       metric.Int64Counter   // attrs: gen_ai.request.model, agent, user, source, role, gen_ai.token.type
	cost             metric.Float64Counter // attrs: gen_ai.request.model, agent, user, source, role; unit USD
	requests         metric.Int64Counter   // attrs: as cost
	runDur           metric.Float64Histogram
	toolCalls        metric.Int64Counter // attrs: agent, role, tool, rejected
	ledgerUnresolved metric.Int64Gauge
}

// metricDef is one instrument's registration facts (name, description, and
// the optional unit/buckets the OTel builder takes positionally).
type metricDef struct {
	name    string
	desc    string
	unit    string
	buckets []float64
}

// assignInstrument builds def's instrument from meter into the *metric.* target, one case per kind.
func assignInstrument(meter metric.Meter, into any, d metricDef) error {
	switch t := into.(type) {
	case *metric.Int64UpDownCounter:
		v, err := meter.Int64UpDownCounter(d.name, metric.WithDescription(d.desc))
		if err != nil {
			return err
		}
		*t = v
	case *metric.Float64Histogram:
		opts := []metric.Float64HistogramOption{metric.WithDescription(d.desc)}
		if d.unit != "" {
			opts = append(opts, metric.WithUnit(d.unit))
		}
		opts = append(opts, metric.WithExplicitBucketBoundaries(d.buckets...))
		v, err := meter.Float64Histogram(d.name, opts...)
		if err != nil {
			return err
		}
		*t = v
	case *metric.Int64Counter:
		opts := []metric.Int64CounterOption{metric.WithDescription(d.desc)}
		// WithUnit("") leaves the unit empty - the unitless case, not a hole.
		opts = append(opts, metric.WithUnit(d.unit))
		v, err := meter.Int64Counter(d.name, opts...)
		if err != nil {
			return err
		}
		*t = v
	case *metric.Float64Counter:
		opts := []metric.Float64CounterOption{metric.WithDescription(d.desc)}
		if d.unit != "" {
			opts = append(opts, metric.WithUnit(d.unit))
		}
		v, err := meter.Float64Counter(d.name, opts...)
		if err != nil {
			return err
		}
		*t = v
	case *metric.Int64Gauge:
		v, err := meter.Int64Gauge(d.name, metric.WithDescription(d.desc))
		if err != nil {
			return err
		}
		*t = v
	}
	return nil
}

// initMetrics builds every instrument and installs the singleton. An error is an OTel SDK bug: callers
// log and continue with metrics disabled.
func initMetrics(meter metric.Meter) error {
	m2 := &metrics{}
	// Registration order is not field order (cost before ledgerUnresolved); the first error short-circuits.
	defs := []struct {
		def  metricDef
		into any
	}{
		{metricDef{"quack.runs.active", "orchestrator runs currently in flight", "", nil}, &m2.runsActive},
		{metricDef{"quack.nodes.active", "DAG nodes currently in flight", "", nil}, &m2.nodesActive},
		{metricDef{"quack.worker.round.duration", "worker round wall time (draft/continuation/revise/hitl/confirm) - derived from the round's own span window, see StartTimedSpan", "s",
			[]float64{1, 2, 5, 10, 30, 60, 120, 300, 600}}, &m2.roundDur},
		{metricDef{"quack.judge.score", "independent judge weakest-link score (0-1); recorded on every agent's shared judge-round path (RunGatedRefine), not just one agent", "",
			[]float64{0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0}}, &m2.judgeScore},
		{metricDef{"quack.judge.verdict", "judge verdicts by pass/fail", "", nil}, &m2.judgeVerdict},
		{metricDef{"quack.judge.unavailable", "judge rounds that errored before producing a verdict (no score/verdict recorded for that round) - a gap here on one agent's series explains missing/sparse quack.judge.score|verdict for it", "", nil}, &m2.judgeUnavailable},
		{metricDef{"quack.judge.phase.duration", "one judge round's wall time per phase (prepare, deterministic, admission, verify, judge_tools, judge_verdict); judge_tools and judge_verdict sum over every judge session in the round, retries and re-judges included", "s",
			[]float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}}, &m2.judgePhase},
		{metricDef{"quack.delivery.outcome", "delivery outcomes; 'none' on a judge-passed work-request that recorded no delivery is the alertable phantom-success regression", "", nil}, &m2.delivery},
		{metricDef{"quack.model.call.duration", "model call duration, swap-sensitive", "s",
			[]float64{1, 2, 5, 10, 30, 60, 120, 300, 600}}, &m2.modelCallDur},
		{metricDef{"quack.acp.permission_ask", "ACP subprocess permission asks reaching the safety judge (should be ~0)", "", nil}, &m2.permAsk},
		{metricDef{"quack.memory.recall", "memory recall attempts, by hit/miss", "", nil}, &m2.memRecall},
		{metricDef{"quack.gate.checks.skipped", "nodes where the deterministic checks criterion did NOT run at all (no backstop), by reason - query this to find nodes that gated on judge score alone", "", nil}, &m2.checksSkipped},
		{metricDef{"quack.memory.commit.failures", "fire-and-forget memory commits that errored (consolidation/embed timeout etc - see RecordMemoryCommitFailure), by reason and agent - the only queryable signal for the M6 commit goroutine, which never fails a node", "", nil}, &m2.memCommitFail},
		{metricDef{"quack.run.no_answer", "runs that finished without hitting the run deadline or being cancelled, yet persisted no final answer - the silent-gap class also covered by gate.checks.skipped/judge.unavailable/delivery.outcome=none, but at the whole-run level (see the GitHub extension's tail-comment fallback)", "", nil}, &m2.runNoAnswer},
		{metricDef{"gen_ai.client.token.usage", "tokens consumed per completed model call, by gen_ai.token.type (input/output/reasoning/cached)", "{token}", nil}, &m2.tokenUsage},
		{metricDef{"quack.ledger.unresolved_intents", "Ledger intents whose projection is missing and boot recovery could not settle", "", nil}, &m2.ledgerUnresolved},
		{metricDef{"gen_ai.client.cost", "USD cost per completed model call, computed from config/quack.yaml's optional per-model price table; a model absent from that table emits no cost here", "USD", nil}, &m2.cost},
		{metricDef{"quack.model.requests", "completed model calls that reported usage (an ACP agent without per-call usage counts once per round), attributed like gen_ai.client.cost - the per-request denominator", "", nil}, &m2.requests},
		{metricDef{"quack.run.duration", "one orchestrator run's wall time; _count is tasks, the cost-per-task denominator", "s",
			[]float64{10, 30, 60, 300, 600, 1800, 3600, 7200, 14400}}, &m2.runDur},
		{metricDef{"quack.tool.calls", "tool calls by tool; rejected=true returned an error to the model (e.g. a write_<kind> schema refusal), costing a recovery request", "", nil}, &m2.toolCalls},
	}
	for _, e := range defs {
		if err := assignInstrument(meter, e.into, e.def); err != nil {
			return err
		}
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

// StartNode opens the "node" span and marks quack.nodes.active +1 as ONE
// call. Pair with EndNode.
func StartNode(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, oteltrace.Span) {
	ctx, span := Start(ctx, "node", attrs...)
	NodeStarted()
	return ctx, span
}

// EndNode ends span and marks quack.nodes.active -1.
func EndNode(span oteltrace.Span, err error) {
	End(span, err)
	NodeFinished()
}

// TimedSpan pairs a span with its start instant so End duration matches the span window.
type TimedSpan struct {
	Span  oteltrace.Span
	start time.Time
}

// StartTimedSpan opens a span and returns a TimedSpan.
func StartTimedSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, *TimedSpan) {
	ctx, span := Start(ctx, name, attrs...)
	return ctx, &TimedSpan{Span: span, start: time.Now()}
}

// End returns wall-clock duration since StartTimedSpan.
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

// RecordJudgeVerdict records one judge round's weakest-link score and pass/fail. firstRound marks one round
// per judged attempt, so rounds per attempt and first-round pass rate are ratios of this one counter.
func RecordJudgeVerdict(agent string, score float64, passed, firstRound bool) {
	if m == nil {
		return
	}
	ctx := context.Background()
	m.judgeScore.Record(ctx, score, metric.WithAttributes(attrAgent(agent)))
	m.judgeVerdict.Add(ctx, 1, metric.WithAttributes(attrAgent(agent), attrBool("passed", passed), attrBool("first_round", firstRound)))
}

// RecordJudgeUnavailable records a judge round that errored before a verdict.
func RecordJudgeUnavailable(agent string) {
	if m == nil {
		return
	}
	m.judgeUnavailable.Add(context.Background(), 1, metric.WithAttributes(attrAgent(agent)))
}

// RecordJudgePhase records one judge round phase's seconds; criterion is "" unless the phase scores one.
func RecordJudgePhase(agent, phase, criterion string, seconds float64) {
	if m == nil {
		return
	}
	attrs := []attribute.KeyValue{attrAgent(agent), attribute.String("phase", phase)}
	if criterion != "" {
		attrs = append(attrs, attribute.String("criterion", criterion))
	}
	m.judgePhase.Record(context.Background(), seconds, metric.WithAttributes(attrs...))
}

// DeliveryOutcome names the outcome recorded by RecordDeliveryOutcome.
type DeliveryOutcome string

const (
	DeliveryDelivered DeliveryOutcome = "delivered"
	DeliveryDraft     DeliveryOutcome = "draft"
	DeliveryFailed    DeliveryOutcome = "failed"
	// DeliveryNone marks a judge-passed work-request with no delivery.
	DeliveryNone DeliveryOutcome = "none"
)

// RecordDeliveryOutcome records one node's delivery outcome.
func RecordDeliveryOutcome(outcome DeliveryOutcome) {
	if m == nil {
		return
	}
	m.delivery.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", string(outcome))))
}

// RecordRunDuration records one finished orchestrator run (a task); defer it with the run's start time.
func RecordRunDuration(start time.Time) {
	if m == nil {
		return
	}
	m.runDur.Record(context.Background(), time.Since(start).Seconds())
}

// RecordToolCall counts one tool call, billed like a model call; rejected means it returned an error.
func RecordToolCall(c ledger.Coords, tool string, rejected bool) {
	if m == nil {
		return
	}
	u := UsageFor("", c, "")
	m.toolCalls.Add(context.Background(), 1, metric.WithAttributes(attrAgent(u.Agent), attribute.String("role", u.Role),
		attribute.String("tool", tool), attrBool("rejected", rejected)))
}

// RecordModelCallDuration records one model call's wall time.
func RecordModelCallDuration(model string, d time.Duration) {
	if m == nil {
		return
	}
	m.modelCallDur.Record(context.Background(), d.Seconds(), metric.WithAttributes(attrModel(model)))
}

// RecordPermissionAsk tracks ACP permission asks reaching the safety judge.
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

// RecordChecksSkipped records nodes where deterministic checks did not run.
func RecordChecksSkipped(reason string) {
	if m == nil {
		return
	}
	m.checksSkipped.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordMemoryCommitFailure records fire-and-forget memory commits that errored.
func RecordMemoryCommitFailure(agent, reason string) {
	if m == nil {
		return
	}
	m.memCommitFail.Add(context.Background(), 1, metric.WithAttributes(attrAgent(agent), attribute.String("reason", reason)))
}

// ClassifyMemoryCommitError buckets an error into a short reason for Grafana.
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

// RecordRunNoAnswer records a run with no persisted answer text.
func RecordRunNoAnswer() {
	if m == nil {
		return
	}
	m.runNoAnswer.Add(context.Background(), 1)
}

// Roles a model call plays; every per-call usage instrument carries one.
const (
	RoleWorker       = "worker"
	RoleJudge        = "judge"
	RoleOrchestrator = "orchestrator"
)

// Usage is one model call's bounded attribution: it never holds a chat, turn or node id.
type Usage struct {
	Model, Agent, User, Source, Role string
}

// UsageFor attributes a call from its coords: a judge call bills the agent it judged, and a call outside any
// node (planning, compaction, memory upkeep) is orchestrator overhead. defaultAgent fills a blank agent.
func UsageFor(model string, c ledger.Coords, defaultAgent string) Usage {
	u := Usage{Model: model, Agent: cmp.Or(c.Agent, defaultAgent), User: c.User, Source: c.Source, Role: RoleWorker}
	switch {
	case c.Agent == ledger.JudgeAgent:
		u.Agent, u.Role = cmp.Or(c.JudgedAgent, c.Agent), RoleJudge
	case c.Node == "":
		u.Role = RoleOrchestrator
	}
	return u
}

// RecordTokenUsage counts the call as one request and records one point per non-zero token type; empty
// attribution is omitted, never faked. input MUST exclude cached tokens or the series double-count.
func RecordTokenUsage(u Usage, input, output, reasoning, cached int64) {
	if m == nil {
		return
	}
	ctx := context.Background()
	m.requests.Add(ctx, 1, metric.WithAttributes(genAIUsageAttrs(u, "")...))
	record := func(tokenType string, n int64) {
		if n == 0 {
			return
		}
		m.tokenUsage.Add(ctx, n, metric.WithAttributes(genAIUsageAttrs(u, tokenType)...))
	}
	record(GenAITokenTypeInput, input)
	record(GenAITokenTypeOutput, output)
	record(GenAITokenTypeReasoning, reasoning)
	record(GenAITokenTypeCached, cached)
}

// RecordCost records gen_ai.client.cost (USD); call it only when the model has a price, since a real $0
// call is indistinguishable from unpriced.
func RecordCost(u Usage, usd float64) {
	if m == nil {
		return
	}
	m.cost.Add(context.Background(), usd, metric.WithAttributes(genAIUsageAttrs(u, "")...))
}

// genAIUsageAttrs omits empty fields. agent stays the bare "agent" label: renaming it breaks dashboards.
func genAIUsageAttrs(u Usage, tokenType string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 6)
	for _, kv := range []attribute.KeyValue{
		attribute.String(GenAIRequestModel, u.Model), attribute.String("agent", u.Agent), attrUser(u.User),
		attrSource(u.Source), attribute.String("role", u.Role), attribute.String(GenAITokenType, tokenType),
	} {
		if kv.Value.AsString() != "" {
			attrs = append(attrs, kv)
		}
	}
	return attrs
}

func attrAgent(v string) attribute.KeyValue          { return attribute.String(GenAIAgentName, v) }
func attrModel(v string) attribute.KeyValue          { return attribute.String(GenAIRequestModel, v) }
func attrStage(v string) attribute.KeyValue          { return attribute.String("stage", v) }
func attrBool(key string, v bool) attribute.KeyValue { return attribute.Bool(key, v) }
func attrUser(v string) attribute.KeyValue           { return attribute.String("user", v) }
func attrSource(v string) attribute.KeyValue         { return attribute.String("source", v) }

// logf for non-fatal startup diagnostics.
func logf(msg string, args ...any) {
	slog.Warn(msg, append([]any{"component", "otelobs"}, args...)...)
}

// SetLedgerUnresolvedIntents publishes the last recovery pass's unresolved count.
func SetLedgerUnresolvedIntents(n int64) {
	if m == nil {
		return
	}
	m.ledgerUnresolved.Record(context.Background(), n)
}
