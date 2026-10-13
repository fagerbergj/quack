package orchestrator

import (
	"context"
	"encoding/json"
	"iter"
	"time"

	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// gatedLLM blocks its worker call on hold, signaling started once entered -
// lets the test observe the gauge while RetryNode is genuinely in-flight.
type gatedLLM struct {
	started chan struct{}
	hold    chan struct{}
}

func (*gatedLLM) Name() string { return "gated" }

func (g *gatedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, "submit_verdict") {
			yield(stubCall("submit_verdict", map[string]any{"score": 3, "feedback": ""}), nil)
			return
		}
		close(g.started)
		<-g.hold
		yield(stubText("done"), nil)
	}
}

// retryNodeMetricsHarness wires one worker node (and its judge) behind gate, with its
// plan already stashed the way RetryNode re-enters it via stashedPlan.
func retryNodeMetricsHarness(t *testing.T, gate model.LLM) (o *Orchestrator, userID, chatID string) {
	t.Helper()
	userID, chatID = "u1", "chat-1"
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: gate, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": gate},
		vetting.NewJudgeFactory(gate, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	o = New(sessions, gate, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)

	plan := dag.Plan{ID: "plan-1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "TASK"}}}
	planJSON, _ := json.Marshal(plan)
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatalf("session create: %v", err)
	}
	return o, userID, chatID
}

// runsActiveGauge reads quack.runs.active's current net value off reader.
func runsActiveGauge(t *testing.T, reader *metric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "quack.runs.active" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("quack.runs.active is not an int64 Sum")
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	t.Fatal("quack.runs.active was never recorded")
	return 0
}

// TestRetryNode_CountsTowardRunsActiveGauge drives RetryNode to completion
// and asserts quack.runs.active goes 0 -> 1 -> 0.
func TestRetryNode_CountsTowardRunsActiveGauge(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}
	// Prime the gauge: an UpDownCounter reports nothing until its first Add.
	otelobs.RunStarted()
	otelobs.RunFinished()

	gate := &gatedLLM{started: make(chan struct{}), hold: make(chan struct{})}
	o, userID, chatID := retryNodeMetricsHarness(t, gate)
	if got := runsActiveGauge(t, reader); got != 0 {
		t.Fatalf("quack.runs.active = %d before the run, want 0", got)
	}
	doneCh := make(chan struct{})
	go func() {
		for ev, err := range o.RetryNode(context.Background(), userID, chatID, "", nil, "n1", "") {
			_ = ev
			if err != nil {
				t.Errorf("retry: %v", err)
			}
		}
		close(doneCh)
	}()
	select {
	case <-gate.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never reached the gate")
	}
	if got := runsActiveGauge(t, reader); got != 1 {
		t.Fatalf("quack.runs.active = %d while the run is in flight, want 1", got)
	}
	close(gate.hold)
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("retry never completed after the gate released")
	}
	if got := runsActiveGauge(t, reader); got != 0 {
		t.Fatalf("quack.runs.active = %d after the run finished, want 0", got)
	}
}

// A retried node's worker and judge calls both bill its agent, told apart by role, and the retry is one task.
func TestRetryNode_BillsWorkerAndJudgeToTheNodeAgent(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}
	m := inference.TracedModelForTesting(funcModel(func(_ context.Context, req *model.LLMRequest) iter.Seq2[*model.LLMResponse, error] {
		return func(yield func(*model.LLMResponse, error) bool) {
			r := stubText("done")
			if stubHasTool(req, "submit_verdict") {
				r = stubCall("submit_verdict", map[string]any{"score": 3, "feedback": ""})
			}
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5}
			yield(r, nil)
		}
	}), "m")
	o, userID, chatID := retryNodeMetricsHarness(t, m)
	for _, err := range o.RetryNode(context.Background(), userID, chatID, "", nil, "n1", "") {
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
	}

	roles := map[string]bool{}
	for _, p := range metricPointsOf(t, reader, "quack.model.requests") {
		if p.Attrs["agent"] != "w" {
			t.Errorf("quack.model.requests point %v bills agent %q, want the node's agent w", p.Attrs, p.Attrs["agent"])
		}
		roles[p.Attrs["role"]] = true
	}
	if !roles[otelobs.RoleWorker] || !roles[otelobs.RoleJudge] {
		t.Errorf("quack.model.requests roles = %v, want worker and judge", roles)
	}
	runs := metricPointsOf(t, reader, "quack.run.duration")
	if len(runs) != 1 || runs[0].Value != 1 {
		t.Errorf("quack.run.duration = %+v, want the retry counted as one task", runs)
	}
	verdicts := metricPointsOf(t, reader, "quack.judge.verdict")
	if len(verdicts) != 1 || verdicts[0].Attrs["first_round"] != "true" {
		t.Errorf("quack.judge.verdict = %+v, want one first-round verdict", verdicts)
	}
}

func metricPointsOf(t *testing.T, reader *metric.ManualReader, name string) []otelobs.MetricPoint {
	t.Helper()
	ps, err := otelobs.PointsForTesting(reader, name)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return ps
}
