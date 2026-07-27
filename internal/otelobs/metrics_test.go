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

// newTestMeter builds a fresh SDK MeterProvider backed by a ManualReader and
// installs its instruments as the package singleton (initMetrics), so the
// public Record*/Start*/End* functions under test record into THIS reader
// rather than whatever a prior test (or Init) left wired up.
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

// sumTotal returns an int64 Sum instrument's current net value - the reading
// an UpDownCounter-backed gauge like quack.runs.active/quack.nodes.active
// exposes with cumulative temporality (the default here).
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
		if v, ok := dp.Attributes.Value(attribute.Key("agent")); ok {
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
		if v, ok := dp.Attributes.Value(attribute.Key("agent")); ok {
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

// TestRunGauge_ReturnsToZero_AfterErroredCancelledAndCleanRuns is #354's
// core regression guard: quack.runs.active must net back to 0 once every
// RunStarted has a matching RunFinished, on EVERY exit shape - a plain error,
// a context-cancellation, and a clean return.
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

// TestRunQueuedGauge_TracksAdmittedButNotYetExecuting is #417's regression
// guard: a run admitted (queued) but not yet holding its concurrency slot
// must show up in quack.runs.queued, NOT quack.runs.active - and the
// queued→active transition must net runs.queued back to 0 as it does so.
func TestRunQueuedGauge_TracksAdmittedButNotYetExecuting(t *testing.T) {
	reader := newTestMeter(t)

	// Prime quack.runs.active so it has a data point to read (an UpDownCounter
	// with no Add call yet produces no data point at all under the SDK's
	// ManualReader, distinct from a genuine 0 reading).
	RunStarted()
	RunFinished()

	RunQueued()
	if got := sumTotal(t, reader, "quack.runs.queued"); got != 1 {
		t.Fatalf("quack.runs.queued = %d after RunQueued, want 1", got)
	}
	if got := sumTotal(t, reader, "quack.runs.active"); got != 0 {
		t.Fatalf("quack.runs.active = %d while only queued (not yet acquired), want 0", got)
	}

	// Acquire a slot: queued -> active.
	RunUnqueued()
	RunStarted()
	if got := sumTotal(t, reader, "quack.runs.queued"); got != 0 {
		t.Errorf("quack.runs.queued = %d after RunUnqueued, want 0", got)
	}
	if got := sumTotal(t, reader, "quack.runs.active"); got != 1 {
		t.Errorf("quack.runs.active = %d after RunStarted, want 1", got)
	}

	RunFinished()
	if got := sumTotal(t, reader, "quack.runs.active"); got != 0 {
		t.Errorf("quack.runs.active = %d after RunFinished, want 0", got)
	}
}

// TestNodeGauge_TracksInFlightThenReturnsToZero exercises concurrency (two
// nodes in flight at once, mirroring the "4 active with 1 serial run"
// production report) and confirms the gauge both reflects the in-flight
// count AND nets to 0 once an errored and a clean node both end.
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

// TestJudgeMetrics_CoverNonExplorerAgents guards #354's item 2: the judge
// score/verdict series must appear for ANY agent whose node reaches the
// shared judge loop, not just one. It also exercises the judge-errored path
// (RecordJudgeUnavailable), which previously left NO metric at all for a
// round the judge failed to score.
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

// TestRoundDuration_MatchesTimedSpanWindow guards #354's item 3: the
// recorded worker-round duration must equal TimedSpan's own window, never a
// blown-up value from a mismatched/independent timer (the reported symptom
// was ~31min recorded for rounds Tempo showed as 4-12min).
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

// TestRecordMemoryCommitFailure guards #436: a fire-and-forget memory-commit
// error must leave a queryable counter series (reason + agent), not just a
// WARN log - the gap the owner flagged after ~every node's commit timed out
// under burst load.
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

// TestRecordRunNoAnswer_Counts guards #568: a run that completes without an
// answer must leave a queryable trace, not just a placeholder comment.
func TestRecordRunNoAnswer_Counts(t *testing.T) {
	reader := newTestMeter(t)
	RecordRunNoAnswer()
	RecordRunNoAnswer()
	if got := sumTotal(t, reader, "quack.run.no_answer"); got != 2 {
		t.Fatalf("quack.run.no_answer = %d, want 2", got)
	}
}

// TestJudgeScoreHistogram_HasExplicitBuckets guards #433: a 0-1 score
// recorded against the OTel default buckets (5, 10, ...) lands entirely in
// one bucket, making the histogram useless. Explicit sub-1.0 boundaries must
// spread scores across multiple buckets.
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
