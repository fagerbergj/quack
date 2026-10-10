package dag

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

func newReviewerAgent(t *testing.T, name string) adkagent.Agent {
	t.Helper()
	a, err := llmagent.New(llmagent.Config{Name: name, Model: fixedLLM("ok", nil), Description: name, Instruction: "review"})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

func noopYield(stream.SSEEvent, error) bool { return true }

// An aborted run's partial fan-in must not leak into a later run of the same plan ID,
// neither its body nor its total.
func TestRunPlanAsGraph_FreshRunResetsStaleReviewFanout(t *testing.T) {
	planID := "plan-stale-" + t.Name()
	stale := vetting.GetReviewFanout(planID, 5)
	stale.Finish("old-r1", vetting.StagedDelivery{Kind: "review", Event: "approve", Body: "STALE FROM ABORTED RUN"}, true, false)

	ex := NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{reviewerAgent: newReviewerAgent(t, reviewerAgent)}, nil, nil,
		func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
	plan := Plan{ID: planID, Nodes: []Node{
		{ID: "r1", AgentName: reviewerAgent},
		{ID: "r2", AgentName: reviewerAgent},
	}}

	// Fresh entry: resumeNodes is nil. Ignore the run's own error/result -
	// buildGateNodes (and its GetReviewFanout call) runs before any node executes.
	_, _ = ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "chat", nil, noopYield, map[string]string{}, nil)

	got := vetting.GetReviewFanout(planID, 2)
	if got == stale {
		t.Fatal("fresh run reused the stale fan-in instance - an aborted run's state can still leak into this run")
	}

	if _, deliver := got.Finish("r1", vetting.StagedDelivery{Kind: "review", Event: "approve", Body: "r1 fresh"}, true, false); deliver {
		t.Fatal("delivered after only 1 of 2 reviewers - total was not reset to this run's own reviewer count")
	}
	merged, deliver := got.Finish("r2", vetting.StagedDelivery{Kind: "review", Event: "approve", Body: "r2 fresh"}, true, false)
	if !deliver {
		t.Fatal("not delivered after both of this run's reviewers finished - stale total (5) still in effect")
	}
	if strings.Contains(merged.Body, "STALE FROM ABORTED RUN") {
		t.Fatalf("merged body carries a previous run's review text: %q", merged.Body)
	}
}

// A resume must not wipe the fan-in, or an already-staged peer reviewer that never calls
// Finish again is dropped from the merge.
func TestRunPlanAsGraph_ResumeDoesNotResetReviewFanout(t *testing.T) {
	planID := "plan-resume-" + t.Name()
	// Finish here bypasses forget(), so the package registry would keep this planID's
	// delivered instance and collide under go test -count>1.
	t.Cleanup(func() { vetting.ResetReviewFanout(planID) })
	fanout := vetting.GetReviewFanout(planID, 2)
	fanout.Finish("r-done", vetting.StagedDelivery{Kind: "review", Event: "approve", Body: "peer already staged"}, true, false)

	ex := NewExecutor(session.InMemoryService(),
		map[string]adkagent.Agent{reviewerAgent: newReviewerAgent(t, reviewerAgent)}, nil, nil,
		func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
	plan := Plan{ID: planID, Nodes: []Node{
		{ID: "r-done", AgentName: reviewerAgent},
		{ID: "r-paused", AgentName: reviewerAgent},
	}}

	// Resume: resumeNodes is non-empty (only r-paused re-runs; r-done durably skips). Cancelled so the
	// graph stops after its gate-node build, before r-paused could deliver and retire the fan-in itself.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = ex.RunPlanAsGraph(ctx, plan, "quack", "u", "chat", nil, noopYield, map[string]string{}, []string{"r-paused"})

	got := vetting.GetReviewFanout(planID, 2)
	if got != fanout {
		t.Fatal("resume reset the fan-in instance - the already-terminal peer's staged verdict is lost, the run can never reach total")
	}
	merged, deliver := got.Finish("r-paused", vetting.StagedDelivery{Kind: "review", Event: "approve", Body: "r-paused resumed"}, true, false)
	if !deliver {
		t.Fatal("resume's fan-in lost track of the already-terminal peer - stuck waiting forever")
	}
	if !strings.Contains(merged.Body, "peer already staged") {
		t.Fatalf("merged body dropped the peer's pre-resume review: %q", merged.Body)
	}
}
