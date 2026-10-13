package vetting

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/otelobs"
)

// A live judge round stores where its time went in the judge_round record and emits each phase as
// quack.judge.phase.duration, so a slow round shows whether reads, the verdict or a re-judge cost it.
func TestJudgeRoundRecordsPhaseTiming(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatal(err)
	}
	var reads int32
	fl := newFakeGateLedger()
	// User "u" is the ADK session user runGatedStub runs as, which the gate records under.
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score", Agent: "web-researcher", ChatID: "chat-timing", User: "u",
		Artifacts: newMetaAwareInMemory(), Ledger: fl}
	judge := NewJudgeFactory(scriptedJudge, []tool.Tool{newSpyReadTool(t, "func TestPlay(t *testing.T) {}", &reads)}, nil)
	if res := runGatedStub(t, fixedScoreModel(1), nil, judge, cfg); !res.Passed {
		t.Fatalf("gate = %+v, want the scripted pass", res)
	}

	rec := loadOnlyJudgeRound(t, cfg, fl)
	if rec.Timing == nil {
		t.Fatal("judge_round record has no timing")
	}
	wantPhases := []string{phaseDeterministic, phaseJudgeTools, phaseJudgeVerdict, phasePrepare, phaseVerify}
	if got := slices.Sorted(maps.Keys(rec.Timing.PhaseSeconds)); !slices.Equal(got, wantPhases) {
		t.Errorf("phases = %v, want %v (no admission without an admission controller)", got, wantPhases)
	}
	if rec.Timing.JudgeCalls != 1 || rec.Timing.ToolCalls != 1 || rec.Timing.JudgeTurns < 2 {
		t.Errorf("timing = %+v, want one judge session with one read over at least two turns", rec.Timing)
	}

	points := phasePoints(t, reader)
	if !slices.Equal(slices.Sorted(maps.Keys(points)), wantPhases) {
		t.Errorf("metric phases = %v, want %v", slices.Sorted(maps.Keys(points)), wantPhases)
	}
	verify, tools := points[phaseVerify], points[phaseJudgeTools]
	if v, _ := verify.Value("criterion"); v.AsString() != specificsSupportedCriterion {
		t.Errorf("verify point criterion = %q, want %q", v.AsString(), specificsSupportedCriterion)
	}
	if v, _ := tools.Value(attribute.Key(otelobs.GenAIAgentName)); v.AsString() != "web-researcher" {
		t.Errorf("judge_tools point agent = %q, want web-researcher", v.AsString())
	}
}

// loadOnlyJudgeRound finds the single judge_round revision the gate wrote and loads its record.
func loadOnlyJudgeRound(t *testing.T, cfg Config, fl *fakeGateLedger) JudgeRoundRecord {
	t.Helper()
	var ids []string
	for _, e := range fl.entries {
		if entryMatchesFailKind(e, "judge_round") {
			ids = append(ids, e.Key)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("judge_round revisions = %v, want exactly one", ids)
	}
	raw, _, _, _, ok, err := recordClient(cfg).LatestWithMeta(context.Background(), ids[0])
	if err != nil || !ok {
		t.Fatalf("load judge_round: ok=%v err=%v", ok, err)
	}
	var rec JudgeRoundRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// phasePoints maps each recorded phase to its data point's attribute set.
func phasePoints(t *testing.T, reader *sdkmetric.ManualReader) map[string]attribute.Set {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]attribute.Set{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			h, ok := m.Data.(metricdata.Histogram[float64])
			if m.Name != "quack.judge.phase.duration" || !ok {
				continue
			}
			for _, dp := range h.DataPoints {
				phase, _ := dp.Attributes.Value("phase")
				out[phase.AsString()] = dp.Attributes
			}
		}
	}
	return out
}
