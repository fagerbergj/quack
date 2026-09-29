package tools

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestPlanShape: renaming ids or rewording tasks keeps a plan's shape; changing an agent, a
// dependency or the delivery kind does not.
func TestPlanShape(t *testing.T) {
	base := PlanShape([]dag.RawNode{
		{ID: "a", Agent: "web-researcher", Task: "look up X"}, {ID: "b", Agent: "web-researcher", Task: "look up Y"},
	}, &dag.Delivery{Kind: "comment"})
	if got := PlanShape([]dag.RawNode{
		{ID: "r2", Agent: "web-researcher", Task: "Y, carefully"}, {ID: "r1", Agent: "web-researcher", Task: "X, carefully"},
	}, &dag.Delivery{Kind: "comment"}); got != base {
		t.Errorf("renamed/reworded shape %q != %q", got, base)
	}
	for name, nodes := range map[string][]dag.RawNode{
		"agent":      {{ID: "a", Agent: "web-researcher"}, {ID: "b", Agent: "code-explorer"}},
		"dependency": {{ID: "a", Agent: "web-researcher"}, {ID: "b", Agent: "web-researcher", DependsOn: []string{"a"}}},
		"node count": {{ID: "a", Agent: "web-researcher"}},
	} {
		if PlanShape(nodes, &dag.Delivery{Kind: "comment"}) == base {
			t.Errorf("a changed %s kept the shape", name)
		}
	}
	if PlanShape([]dag.RawNode{{ID: "a", Agent: "web-researcher"}, {ID: "b", Agent: "web-researcher"}}, nil) == base {
		t.Error("dropping delivery kept the shape")
	}
}

// TestRecordRejection_LoopGuard: the guard trips on the second rejection of the same shape and at
// the per-turn cap, never on genuinely different plans below it.
func TestRecordRejection_LoopGuard(t *testing.T) {
	c := NewPlanCache()
	if c.RecordRejection("r1", "s1") || c.RecordRejection("r2", "s2") {
		t.Fatal("two different plans tripped the guard")
	}
	if !c.RecordRejection("r3", "s3") {
		t.Errorf("rejection %d did not trip the cap", MaxPlanRejections)
	}
	c = NewPlanCache()
	c.RecordRejection("first", "s1")
	if !c.RecordRejection("second", "s1") {
		t.Fatal("the same shape rejected twice did not trip the guard")
	}
	if reason, tripped := c.LoopGuard(); !tripped || reason != "second" {
		t.Errorf("LoopGuard = %q %v, want the latest reason, tripped", reason, tripped)
	}
}

// TestExecuteTool_LoopGuardEndsTurn: re-running a plan the judge already rejected - only its task
// reworded - ends the turn for the user's choice instead of failing into another re-plan.
func TestExecuteTool_LoopGuardEndsTurn(t *testing.T) {
	judge := vetting.PlanJudge(func(context.Context, string, string, string) (bool, string, error) {
		return false, "the user asked for no synthesizer", nil
	})
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, judge)
	cache := NewPlanCache()
	run := func(task string) (map[string]any, *execToolCtx, error) {
		rec := dag.DagPlanRecord{PlanID: "p1", Assignments: []dag.Assignment{{NodeID: "r-1", Task: task}}}
		c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "r-1", Agent: "web-researcher"}})
		tl, err := NewExecuteTool(planner, c, cache, nil, nil, nil, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := newExecToolCtx()
		out, err := tl.(runnableTool).Run(ctx, map[string]any{"plan_id": "p1"})
		return out, ctx, err
	}
	if _, ctx, err := run("research it"); err == nil || ctx.actions.SkipSummarization {
		t.Fatalf("first rejection: err=%v skip=%v, want a tool error the model can react to", err, ctx.actions.SkipSummarization)
	}
	out, ctx, err := run("research it, more carefully")
	if err != nil || out["status"] != "needs_user" || !ctx.actions.SkipSummarization {
		t.Fatalf("second rejection: out=%v err=%v skip=%v, want needs_user ending the turn", out, err, ctx.actions.SkipSummarization)
	}
}
