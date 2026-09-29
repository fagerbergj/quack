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

// TestExecuteTool_StoppedSinkInPartialStepContinues: with no delivery declared there is no
// answer to lose, so stopping either of two independent sinks just reports it and the turn goes on.
func TestExecuteTool_StoppedSinkInPartialStepContinues(t *testing.T) {
	for _, stopped := range []string{"a-1", "b-1"} {
		t.Run(stopped, func(t *testing.T) {
			rec := dag.DagPlanRecord{PlanID: "p1", Assignments: []dag.Assignment{{NodeID: "a-1", Task: "a"}, {NodeID: "b-1", Task: "b"}}}
			planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
			c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "a-1", Agent: "web-researcher"}, {NodeID: "b-1", Agent: "web-researcher"}})
			step := &fakeRunStep{outputs: map[string]string{"a-1": "A", "b-1": "B"}}
			tl, err := NewExecuteTool(planner, c, NewPlanCache(), nil, step.run, nil, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := newExecToolCtx()
			ctx.Ctx = WithNodeStopped(context.Background(), func(id string) bool { return id == stopped })
			out, err := tl.(runnableTool).Run(ctx, map[string]any{"plan_id": "p1"})
			if err != nil {
				t.Fatal(err)
			}
			if out["status"] != "running" || ctx.actions.SkipSummarization || resultFor(t, out, stopped)["status"] != "cancelled" {
				t.Errorf("status = %v skip=%v, want a running step with %s cancelled", out["status"], ctx.actions.SkipSummarization, stopped)
			}
		})
	}
}

// TestExecuteTool_StoppedDraftNeverDeliveredLater: a draft stopped in an earlier turn stays
// out of delivery when a later step declares one, though the executor's stop flag is gone.
func TestExecuteTool_StoppedDraftNeverDeliveredLater(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "a-1", Task: "a", TaskID: "t-a", Result: "STOPPED DRAFT", Stopped: true},
			{NodeID: "b-1", Task: "b"},
		},
		Delivery: &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "a-1", Agent: "web-researcher"}, {NodeID: "b-1", Agent: "web-researcher"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"b-1": "B"}}
	finalize := func(_ context.Context, plan dag.Plan, outputs map[string]string) string {
		return TerminalOutput(plan, outputs)
	}
	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, finalize, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.(runnableTool).Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatal(err)
	}
	if got := cache.Delivered(); got == "STOPPED DRAFT" {
		t.Errorf("delivered the stopped draft %q from an earlier turn", got)
	}
}
