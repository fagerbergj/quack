package tools

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

// stoppedStepRun runs a,b -> s with b stopped by the user mid-review (its draft still produced).
func stoppedStepRun(t *testing.T, stopped string) (map[string]any, *execToolCtx, *PlanCache, *bool) {
	t.Helper()
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "a-1", Task: "a"}, {NodeID: "b-1", Task: "b"},
			{NodeID: "s-1", Task: "synthesize", DependsOn: []string{"a-1", "b-1"}},
		},
		Delivery: &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "a-1", Agent: "web-researcher"}, {NodeID: "b-1", Agent: "web-researcher"}, {NodeID: "s-1", Agent: "synthesizer"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"a-1": "A", "b-1": "B DRAFT", "s-1": "FINAL"}}
	finalized := false
	finalize := func(context.Context, dag.Plan, map[string]string) string { finalized = true; return "THE ANSWER" }
	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, finalize, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := newExecToolCtx()
	ctx.Ctx = WithNodeStopped(context.Background(), func(id string) bool { return id == stopped })
	out, err := tl.(runnableTool).Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	return out, ctx, cache, &finalized
}

func resultFor(t *testing.T, out map[string]any, nodeID string) map[string]any {
	t.Helper()
	list, _ := out["results"].([]any)
	for _, r := range list {
		if m, _ := r.(map[string]any); m["node_id"] == nodeID {
			return m
		}
	}
	t.Fatalf("no result for %s in %#v", nodeID, out["results"])
	return nil
}

// TestExecuteTool_StoppedSiblingStillDelivers: a user-stopped sibling reports "cancelled"
// with a do-not-redo summary, not "failed", so the done terminal node still delivers.
func TestExecuteTool_StoppedSiblingStillDelivers(t *testing.T) {
	out, ctx, cache, finalized := stoppedStepRun(t, "b-1")
	b := resultFor(t, out, "b-1")
	if b["status"] != "cancelled" || b["summary"] != stoppedSummary {
		t.Errorf("b-1 result = %+v, want cancelled with the stopped summary", b)
	}
	if out["status"] != "delivered" || !*finalized || cache.Delivered() != "THE ANSWER" || !ctx.actions.SkipSummarization {
		t.Errorf("status = %v finalized=%v delivered=%q skip=%v, want a delivered step", out["status"], *finalized, cache.Delivered(), ctx.actions.SkipSummarization)
	}
}

// TestExecuteTool_StoppedTerminalEndsTurnWithoutAnswer: stopping the terminal node leaves
// only a draft - nothing is finalized or delivered, and the turn ends instead of re-planning.
func TestExecuteTool_StoppedTerminalEndsTurnWithoutAnswer(t *testing.T) {
	out, ctx, cache, finalized := stoppedStepRun(t, "s-1")
	if out["status"] != "stopped" || *finalized || cache.Delivered() != "" || !ctx.actions.SkipSummarization {
		t.Errorf("status = %v finalized=%v delivered=%q skip=%v, want stopped, undelivered, turn ended", out["status"], *finalized, cache.Delivered(), ctx.actions.SkipSummarization)
	}
}
