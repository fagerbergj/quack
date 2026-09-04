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

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestRetryNodeResumedSkipsRunAdmission pins #1176: a boot resume rides
// RetryNodeResumed, which must run even while the one run slot is held by
// another run - the resumed node was already admitted by the process that
// died, and that reservation is gone, so re-acquiring here would starve new
// work out of the admission queue forever.
func TestRetryNodeResumedSkipsRunAdmission(t *testing.T) {
	o := &Orchestrator{sessions: session.InMemoryService()}
	o.SetMaxActiveRuns(1)

	hold, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the one slot")
	}
	defer hold()

	done := make(chan struct{})
	go func() {
		// No stashed plan exists, so this yields an error and returns fast -
		// what matters is that it never blocks waiting on the held slot.
		for range o.RetryNodeResumed(context.Background(), "u1", "c1", nil, "n1", "") {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RetryNodeResumed blocked on the run admission slot - it must skip acquireRun")
	}
}

// TestRetryNodeStillConsumesRunAdmission is TestRetryNodeResumedSkipsRunAdmission's
// counterpart: a REST-triggered retry (RetryNode, not RetryNodeResumed) is a
// fresh dispatch and must still wait for a slot like any other run.
func TestRetryNodeStillConsumesRunAdmission(t *testing.T) {
	o := &Orchestrator{sessions: session.InMemoryService()}
	o.SetMaxActiveRuns(1)

	hold, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the one slot")
	}

	done := make(chan struct{})
	go func() {
		for range o.RetryNode(context.Background(), "u1", "c1", nil, "n1", "") {
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("RetryNode returned before the slot freed - it is not going through admission")
	case <-time.After(100 * time.Millisecond):
		// correctly blocked waiting on the held slot
	}
	hold()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RetryNode never proceeded after the slot freed")
	}
}

// gatedLLM blocks its worker call on hold, and signals started once entered -
// lets the test observe quack.runs.active while retryNode is genuinely
// in-flight, not just before/after.
type gatedLLM struct {
	started chan struct{}
	hold    chan struct{}
}

func (*gatedLLM) Name() string { return "gated" }

func (g *gatedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, "submit_verdict") {
			yield(stubCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		close(g.started)
		<-g.hold
		yield(stubText("done"), nil)
	}
}

// retryNodeMetricsHarness wires one worker node behind a gatedLLM, with its
// plan already stashed the way the execute tool leaves it - the shape
// RetryNode/RetryNodeResumed both re-enter via stashedPlan.
func retryNodeMetricsHarness(t *testing.T) (o *Orchestrator, gate *gatedLLM, userID, chatID string) {
	t.Helper()
	userID, chatID = "u1", "chat-1"
	gate = &gatedLLM{started: make(chan struct{}), hold: make(chan struct{})}
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: gate, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": gate},
		vetting.NewJudgeFactory(gate, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	o = New(sessions, gate, "", nil, ex, nil, nil, nil)

	plan := dag.Plan{ID: "plan-1", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "TASK"}}}
	planJSON, _ := json.Marshal(plan)
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatalf("session create: %v", err)
	}
	return o, gate, userID, chatID
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

// TestRetryNodeAndRetryNodeResumed_CountTowardRunsActiveGauge drives BOTH
// entrypoints to completion through retryNode itself (not the otelobs
// primitives directly, #1176 review) and asserts quack.runs.active goes
// 0 -> 1 -> 0 for each - would catch RunStarted/RunFinished being moved
// inside the `if admit` block, which a primitives-only test cannot.
func TestRetryNodeAndRetryNodeResumed_CountTowardRunsActiveGauge(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}
	// Prime the gauge so it has a data point before any assertion reads it -
	// an UpDownCounter reports nothing until its first Add (metrics_test.go's
	// own pattern).
	otelobs.RunStarted()
	otelobs.RunFinished()

	type retryFunc func(o *Orchestrator, userID, chatID, nodeID string) iter.Seq2[stream.SSEEvent, error]

	drive := func(t *testing.T, retry retryFunc) {
		o, gate, userID, chatID := retryNodeMetricsHarness(t)
		if got := runsActiveGauge(t, reader); got != 0 {
			t.Fatalf("quack.runs.active = %d before the run, want 0", got)
		}
		doneCh := make(chan struct{})
		go func() {
			for ev, err := range retry(o, userID, chatID, "n1") {
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

	t.Run("RetryNode", func(t *testing.T) {
		drive(t, func(o *Orchestrator, userID, chatID, nodeID string) iter.Seq2[stream.SSEEvent, error] {
			return o.RetryNode(context.Background(), userID, chatID, nil, nodeID, "")
		})
	})
	t.Run("RetryNodeResumed", func(t *testing.T) {
		drive(t, func(o *Orchestrator, userID, chatID, nodeID string) iter.Seq2[stream.SSEEvent, error] {
			return o.RetryNodeResumed(context.Background(), userID, chatID, nil, nodeID, "")
		})
	})
}
