package otelobs

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMeter installs a ManualReader-backed singleton so Record*/Start*/End* record into THIS reader,
// not whatever a prior test left wired.
func newTestMeter(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := initMetrics(mp.Meter("test")); err != nil {
		t.Fatalf("initMetrics: %v", err)
	}
	return reader
}

// Pins every unit-carrying instrument's unit; the Int64Counter case once silently dropped d.unit.
func TestInstrumentUnits(t *testing.T) {
	reader := newTestMeter(t)
	RecordTokenUsage("model-a", "", "", "", 1, 1, 0, 0)
	RecordCost("model-a", "", "", "", 0.01)
	for name, want := range map[string]string{
		"gen_ai.client.token.usage": "{token}",
		"gen_ai.client.cost":        "USD",
	} {
		t.Run(name, func(t *testing.T) {
			if got := collect(t, reader, name).Unit; got != want {
				t.Fatalf("unit = %q, want %q", got, want)
			}
		})
	}
}

func collect(t *testing.T, reader *metric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name == name {
				return met
			}
		}
	}
	t.Fatalf("metric %q was never recorded", name)
	return metricdata.Metrics{}
}

// sumTotal returns an int64 Sum's net value, which an UpDownCounter gauge exposes cumulatively.
func sumTotal(t *testing.T, reader *metric.ManualReader, name string) int64 {
	t.Helper()
	sum, ok := collect(t, reader, name).Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 Sum", name)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func sumAgents(t *testing.T, reader *metric.ManualReader, name string) map[string]bool {
	t.Helper()
	sum, ok := collect(t, reader, name).Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q is not an int64 Sum", name)
	}
	out := map[string]bool{}
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key("gen_ai.agent.name")); ok {
			out[v.AsString()] = true
		}
	}
	return out
}

func histogramAgents(t *testing.T, reader *metric.ManualReader, name string) map[string]bool {
	t.Helper()
	h, ok := collect(t, reader, name).Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is not a float64 Histogram", name)
	}
	out := map[string]bool{}
	for _, dp := range h.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key("gen_ai.agent.name")); ok {
			out[v.AsString()] = true
		}
	}
	return out
}

func histogramSumCount(t *testing.T, reader *metric.ManualReader, name string) (sum float64, count uint64) {
	t.Helper()
	h, ok := collect(t, reader, name).Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q is not a float64 Histogram", name)
	}
	for _, dp := range h.DataPoints {
		sum += dp.Sum
		count += dp.Count
	}
	return sum, count
}

// quack.runs.active must net to 0 once every RunStarted has a RunFinished: error, cancellation, or clean.
func TestRunGauge_ReturnsToZero_AfterErroredCancelledAndCleanRuns(t *testing.T) {
	reader := newTestMeter(t)

	runOnce := func(err error) {
		_, span := Start(context.Background(), "run", attribute.String(ChatIDKey, "chat-1"))
		RunStarted()
		defer func() {
			RunFinished()
			End(span, err)
		}()
	}
	runOnce(errors.New("boom"))
	runOnce(context.Canceled)
	runOnce(nil)

	if got := sumTotal(t, reader, "quack.runs.active"); got != 0 {
		t.Errorf("quack.runs.active = %d after 3 matched start/end pairs (error, cancel, clean), want 0", got)
	}
}

// TestRunGauge_CountsAResumedRun pins that a boot-resumed node still counts
// toward quack.runs.active, or the metric undercounts load.
func TestRunGauge_CountsAResumedRun(t *testing.T) {
	reader := newTestMeter(t)

	RunStarted()
	if got := sumTotal(t, reader, "quack.runs.active"); got != 1 {
		t.Fatalf("quack.runs.active = %d after RunStarted, want 1", got)
	}
	RunFinished()
	if got := sumTotal(t, reader, "quack.runs.active"); got != 0 {
		t.Errorf("quack.runs.active = %d after RunFinished, want 0", got)
	}
}

// Two nodes in flight at once: the gauge reflects the in-flight count and nets to 0 once both end.
func TestNodeGauge_TracksInFlightThenReturnsToZero(t *testing.T) {
	reader := newTestMeter(t)

	_, span1 := StartNode(context.Background(), attribute.String("node_id", "n1"))
	_, span2 := StartNode(context.Background(), attribute.String("node_id", "n2"))
	if got := sumTotal(t, reader, "quack.nodes.active"); got != 2 {
		t.Fatalf("quack.nodes.active = %d with 2 nodes started, want 2", got)
	}

	EndNode(span1, errors.New("worker failed"))
	if got := sumTotal(t, reader, "quack.nodes.active"); got != 1 {
		t.Fatalf("quack.nodes.active = %d after 1 of 2 nodes ended (errored), want 1", got)
	}

	EndNode(span2, nil)
	if got := sumTotal(t, reader, "quack.nodes.active"); got != 0 {
		t.Fatalf("quack.nodes.active = %d after both nodes ended, want 0", got)
	}
}

// Judge score/verdict series appear for any agent reaching the judge loop, and RecordJudgeUnavailable
// leaves a metric for an unscored round.
func TestJudgeMetrics_CoverNonExplorerAgents(t *testing.T) {
	reader := newTestMeter(t)

	RecordJudgeVerdict("web-researcher", 0.82, true)
	RecordJudgeVerdict("synthesizer", 0.55, false)
	RecordJudgeUnavailable("code-reviewer")

	scoreAgents := histogramAgents(t, reader, "quack.judge.score")
	for _, want := range []string{"web-researcher", "synthesizer"} {
		if !scoreAgents[want] {
			t.Errorf("quack.judge.score has no series for agent=%q (got agents %v)", want, scoreAgents)
		}
	}

	verdictAgents := sumAgents(t, reader, "quack.judge.verdict")
	for _, want := range []string{"web-researcher", "synthesizer"} {
		if !verdictAgents[want] {
			t.Errorf("quack.judge.verdict has no series for agent=%q (got agents %v)", want, verdictAgents)
		}
	}

	unavailAgents := sumAgents(t, reader, "quack.judge.unavailable")
	if !unavailAgents["code-reviewer"] {
		t.Errorf("quack.judge.unavailable has no series for agent=%q (got agents %v)", "code-reviewer", unavailAgents)
	}
}

// The recorded worker-round duration must equal TimedSpan's own window, never an independent timer's.
func TestRoundDuration_MatchesTimedSpanWindow(t *testing.T) {
	reader := newTestMeter(t)

	_, ts := StartTimedSpan(context.Background(), "worker.round", attribute.String("stage", "draft"))
	time.Sleep(20 * time.Millisecond)
	d := ts.End(nil)
	RecordRoundDuration("web-researcher", "qwen3", "draft", d)

	if d > time.Second {
		t.Fatalf("TimedSpan window = %v for a 20ms sleep - inflated exactly like #354's reported 31min/4-12min mismatch", d)
	}

	sum, count := histogramSumCount(t, reader, "quack.worker.round.duration")
	if count != 1 {
		t.Fatalf("quack.worker.round.duration count = %d, want 1", count)
	}
	if diff := math.Abs(sum - d.Seconds()); diff > 0.005 {
		t.Errorf("quack.worker.round.duration recorded %.6fs, want it to equal the span's own window %.6fs (diff %.6fs)", sum, d.Seconds(), diff)
	}
}

// A fire-and-forget memory-commit error must leave a queryable counter series (reason + agent), not just a log.
func TestRecordMemoryCommitFailure(t *testing.T) {
	reader := newTestMeter(t)

	RecordMemoryCommitFailure("explore-quack", "consolidation")
	RecordMemoryCommitFailure("explore-quack", "embed_writes")

	agents := sumAgents(t, reader, "quack.memory.commit.failures")
	if !agents["explore-quack"] {
		t.Errorf("quack.memory.commit.failures has no series for agent=%q (got agents %v)", "explore-quack", agents)
	}
	if total := sumTotal(t, reader, "quack.memory.commit.failures"); total != 2 {
		t.Errorf("quack.memory.commit.failures total = %d, want 2", total)
	}
}

func TestClassifyMemoryCommitError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("memory: consolidation model: context deadline exceeded"), "consolidation"},
		{errors.New("memory: embed for neighbours: context deadline exceeded"), "embed_neighbours"},
		{errors.New("memory: embed writes: context deadline exceeded"), "embed_writes"},
		{errors.New("memory: something else broke"), "other"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := ClassifyMemoryCommitError(c.err); got != c.want {
			t.Errorf("ClassifyMemoryCommitError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A run that completes without an answer must leave a queryable trace, not just a placeholder comment.
func TestRecordRunNoAnswer_Counts(t *testing.T) {
	reader := newTestMeter(t)
	RecordRunNoAnswer()
	RecordRunNoAnswer()
	if got := sumTotal(t, reader, "quack.run.no_answer"); got != 2 {
		t.Fatalf("quack.run.no_answer = %d, want 2", got)
	}
}

// tokenUsagePoints collects gen_ai.client.token.usage data points keyed by
// their gen_ai.token.type attribute (each type is its own series).
func tokenUsagePoints(t *testing.T, reader *metric.ManualReader) map[string]metricdata.DataPoint[int64] {
	t.Helper()
	sum, ok := collect(t, reader, "gen_ai.client.token.usage").Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
	}
	out := map[string]metricdata.DataPoint[int64]{}
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key(GenAITokenType))
		out[v.AsString()] = dp
	}
	return out
}

func attrString(t *testing.T, set attribute.Set, key string) string {
	t.Helper()
	v, _ := set.Value(attribute.Key(key))
	return v.AsString()
}

// TestRecordTokenUsage_FansOutByTokenType guards the four-way split: one
// data point per non-zero token_type, each carrying model/agent/user/source.
func TestRecordTokenUsage_FansOutByTokenType(t *testing.T) {
	reader := newTestMeter(t)

	RecordTokenUsage("qwen3-coder", "code-implementer", "local", "github", 100, 50, 10, 25)

	points := tokenUsagePoints(t, reader)
	for typ, want := range map[string]int64{
		GenAITokenTypeInput:     100,
		GenAITokenTypeOutput:    50,
		GenAITokenTypeReasoning: 10,
		GenAITokenTypeCached:    25,
	} {
		dp, ok := points[typ]
		if !ok {
			t.Fatalf("no data point for token_type=%q (got %v)", typ, points)
		}
		if dp.Value != want {
			t.Errorf("token_type=%q value = %d, want %d", typ, dp.Value, want)
		}
		if got := attrString(t, dp.Attributes, GenAIRequestModel); got != "qwen3-coder" {
			t.Errorf("token_type=%q gen_ai.request.model = %q, want %q", typ, got, "qwen3-coder")
		}
		if got := attrString(t, dp.Attributes, "agent"); got != "code-implementer" {
			t.Errorf("token_type=%q agent = %q, want %q", typ, got, "code-implementer")
		}
		if got := attrString(t, dp.Attributes, "user"); got != "local" {
			t.Errorf("token_type=%q user = %q, want %q", typ, got, "local")
		}
		if got := attrString(t, dp.Attributes, "source"); got != "github" {
			t.Errorf("token_type=%q source = %q, want %q", typ, got, "github")
		}
	}
}

// TestRecordTokenUsage_ZeroTypeOmitted guards against a spurious series for
// a token type that genuinely had zero tokens this call (e.g. no cache hit).
func TestRecordTokenUsage_ZeroTypeOmitted(t *testing.T) {
	reader := newTestMeter(t)

	RecordTokenUsage("qwen3-coder", "web-researcher", "local", "app", 100, 50, 0, 0)

	points := tokenUsagePoints(t, reader)
	if _, ok := points[GenAITokenTypeReasoning]; ok {
		t.Errorf("token_type=reasoning has a data point for a zero-token call, want none")
	}
	if _, ok := points[GenAITokenTypeCached]; ok {
		t.Errorf("token_type=cached has a data point for a zero-token call, want none")
	}
}

// An empty agent/user/source must never appear as an empty-string attribute value.
func TestRecordTokenUsage_AbsentAttributionOmitsAttribute(t *testing.T) {
	reader := newTestMeter(t)

	RecordTokenUsage("qwen3-coder", "", "", "", 10, 0, 0, 0)

	points := tokenUsagePoints(t, reader)
	dp, ok := points[GenAITokenTypeInput]
	if !ok {
		t.Fatal("no data point for token_type=input")
	}
	for _, key := range []string{"agent", "user", "source"} {
		if _, ok := dp.Attributes.Value(attribute.Key(key)); ok {
			t.Errorf("attribute %q is set on an unattributed call, want it omitted entirely", key)
		}
	}
}

// Pins RecordCost's attribute/value contract; the price math lives in inference.recordUsageMetrics.
func TestRecordCost_ComputesFromConfiguredPricing(t *testing.T) {
	reader := newTestMeter(t)

	RecordCost("qwen3-coder", "code-implementer", "local", "github", 1.5)

	h, ok := collect(t, reader, "gen_ai.client.cost").Data.(metricdata.Sum[float64])
	if !ok {
		t.Fatalf("gen_ai.client.cost is not a float64 Sum")
	}
	if len(h.DataPoints) != 1 {
		t.Fatalf("gen_ai.client.cost has %d data points, want 1", len(h.DataPoints))
	}
	dp := h.DataPoints[0]
	if dp.Value != 1.5 {
		t.Errorf("gen_ai.client.cost value = %v, want 1.5", dp.Value)
	}
	if got := attrString(t, dp.Attributes, GenAIRequestModel); got != "qwen3-coder" {
		t.Errorf("gen_ai.client.cost gen_ai.request.model = %q, want %q", got, "qwen3-coder")
	}
	if _, ok := dp.Attributes.Value(attribute.Key(GenAITokenType)); ok {
		t.Errorf("gen_ai.client.cost carries gen_ai.token.type, want cost to stay a single per-call total")
	}
}

// A 0-1 score on OTel's default buckets lands in one bucket; explicit sub-1.0 boundaries must spread it.
func TestJudgeScoreHistogram_HasExplicitBuckets(t *testing.T) {
	reader := newTestMeter(t)
	RecordJudgeVerdict("web-researcher", 0.3, false)
	RecordJudgeVerdict("web-researcher", 0.9, true)

	h, ok := collect(t, reader, "quack.judge.score").Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("quack.judge.score is not a float64 Histogram")
	}
	for _, dp := range h.DataPoints {
		if len(dp.Bounds) < 5 || dp.Bounds[len(dp.Bounds)-1] > 1.0 {
			t.Errorf("quack.judge.score bucket bounds = %v, want explicit sub-1.0 boundaries", dp.Bounds)
		}
	}
}
