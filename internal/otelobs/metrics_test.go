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

// sumTotal returns an int64 Sum instrument's current net value — the reading
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
// StartRun has a matching EndRun, on EVERY exit shape — a plain error, a
// context-cancellation, and a clean return.
func TestRunGauge_ReturnsToZero_AfterErroredCancelledAndCleanRuns(t *testing.T) {
	reader := newTestMeter(t)

	runOnce := func(err error) {
		_, span := StartRun(context.Background(), attribute.String(ChatIDKey, "chat-1"))
		defer EndRun(span, err)
	}
	runOnce(errors.New("boom"))
	runOnce(context.Canceled)
	runOnce(nil)

	if got := sumTotal(t, reader, "quack.runs.active"); got != 0 {
		t.Errorf("quack.runs.active = %d after 3 matched start/end pairs (error, cancel, clean), want 0", got)
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
		t.Fatalf("TimedSpan window = %v for a 20ms sleep — inflated exactly like #354's reported 31min/4-12min mismatch", d)
	}

	sum, count := histogramSumCount(t, reader, "quack.worker.round.duration")
	if count != 1 {
		t.Fatalf("quack.worker.round.duration count = %d, want 1", count)
	}
	if diff := math.Abs(sum - d.Seconds()); diff > 0.005 {
		t.Errorf("quack.worker.round.duration recorded %.6fs, want it to equal the span's own window %.6fs (diff %.6fs)", sum, d.Seconds(), diff)
	}
}
