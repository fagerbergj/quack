package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
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
	ctx.Ctx = WithNodeStopped(context.Background(), func(id string) bool { return step.calls > 0 && id == stopped }) // stopped while the step ran
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

// Stopping the terminal node leaves only a draft: nothing is delivered, and the turn ends instead of
// re-planning.
func TestExecuteTool_StoppedTerminalEndsTurnWithoutAnswer(t *testing.T) {
	out, ctx, cache, finalized := stoppedStepRun(t, "s-1")
	if out["status"] != "stopped" || *finalized || cache.Delivered() != "" || !ctx.actions.SkipSummarization {
		t.Errorf("status = %v finalized=%v delivered=%q skip=%v, want stopped, undelivered, turn ended", out["status"], *finalized, cache.Delivered(), ctx.actions.SkipSummarization)
	}
}

// With no delivery declared there is no answer to lose, so a stopped sink is just reported and the turn
// goes on.
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
			ctx.Ctx = WithNodeStopped(context.Background(), func(id string) bool { return step.calls > 0 && id == stopped }) // stopped while the step ran
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
	finalize := answerOf
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

// TestExecuteTool_ExtensionDeliversItsOwnSink: a turn extending a delivered plan with a new
// delivering node delivers that node's answer, not the plan's older first sink.
func TestExecuteTool_ExtensionDeliversItsOwnSink(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "s-1", Task: "synthesize", TaskID: "t-s", Result: "TURN-1 TABLE"},
			{NodeID: "w-4", Task: "look it up"},
		},
		Delivery: &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "s-1", Agent: "synthesizer"}, {NodeID: "w-4", Agent: "web-researcher"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"w-4": "W4 ANSWER"}}
	finalize := answerOf
	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, finalize, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.(runnableTool).Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatal(err)
	}
	if got := cache.Delivered(); got != "W4 ANSWER" {
		t.Errorf("delivered %q, want the extension's own answer", got)
	}
}

// TestPlanTools_RefuseReusingStoppedNode: within the turn the user stopped a node, the model
// can't reassign it (edit_plan/create_plan) or re-run it (execute).
func TestPlanTools_RefuseReusingStoppedNode(t *testing.T) {
	ctx := WithNodeStopped(context.Background(), func(id string) bool { return id == "b-1" })
	if err := refuseStopped(ctx, []assignmentInput{{NodeID: "a-1"}, {NodeID: "b-1", Task: "try again"}}); err == nil || !strings.Contains(err.Error(), "stopped by the user") {
		t.Errorf("refuseStopped = %v, want the stopped-node refusal", err)
	}
	if err := refuseStopped(ctx, []assignmentInput{{Agent: "web-researcher", Task: "new work"}}); err != nil {
		t.Errorf("a fresh hire was refused: %v", err)
	}
	rec := dag.DagPlanRecord{PlanID: "p1", Assignments: []dag.Assignment{{NodeID: "b-1", Task: "again"}}}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "b-1", Agent: "web-researcher"}})
	step := &fakeRunStep{outputs: map[string]string{"b-1": "B AGAIN"}}
	tl, err := NewExecuteTool(planner, c, NewPlanCache(), nil, step.run, nil, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	ectx := newExecToolCtx()
	ectx.Ctx = ctx
	if _, err := tl.(runnableTool).Run(ectx, map[string]any{"plan_id": "p1"}); err == nil || step.calls != 0 {
		t.Errorf("execute err=%v calls=%d, want a refusal before anything ran", err, step.calls)
	}
}

// TestDeliverableResults_MasksStoppedDraft: a stopped assignment's draft is never a result a
// delivery can use; every other result passes through.
func TestDeliverableResults_MasksStoppedDraft(t *testing.T) {
	got, stopped := DeliverableResults([]dag.Assignment{
		{NodeID: "a", Result: "REVIEWED"},
		{NodeID: "b", Result: "DRAFT", Stopped: true},
	})
	if got["a"] != "REVIEWED" || got["b"] != "" || stopped["a"] || !stopped["b"] {
		t.Errorf("DeliverableResults = %v %v, want a kept and b masked and flagged", got, stopped)
	}
	if _, ok := got["b"]; !ok {
		t.Error("the stopped node is missing - DeliveredAnswer would fall back to another node's output")
	}
}

// answerOf finalizes like the orchestrator: every sink present, a live-stopped one masked.
func answerOf(ctx context.Context, plan dag.Plan, outputs map[string]string) string {
	answer, _ := DeliveredAnswer(plan, outputs, NodeStoppedFromContext(ctx))
	return answer
}

// multiSinkRun executes two independent delivering researchers, a-1 and b-1, with stopped
// flagging the ones the user stopped while the step ran.
func multiSinkRun(t *testing.T, stopped ...string) (map[string]any, *PlanCache) {
	t.Helper()
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "a-1", Task: "a"}, {NodeID: "b-1", Task: "b"}},
		Delivery:    &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "a-1", Agent: "web-researcher"}, {NodeID: "b-1", Agent: "web-researcher"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"a-1": "A FINDINGS", "b-1": "B FINDINGS"}}
	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, answerOf, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := newExecToolCtx()
	ctx.Ctx = WithNodeStopped(context.Background(), func(id string) bool { return step.calls > 0 && slices.Contains(stopped, id) })
	out, err := tl.(runnableTool).Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	return out, cache
}

// TestExecuteTool_MultiSinkDelivery: two sinks and no synthesizer deliver both outputs as labelled
// sections; a stopped sink ships only a note, and with every sink stopped nothing is delivered.
func TestExecuteTool_MultiSinkDelivery(t *testing.T) {
	out, cache := multiSinkRun(t)
	want := "## a-1\n\nA FINDINGS\n\n## b-1\n\nB FINDINGS"
	if out["status"] != "delivered" || cache.Delivered() != want {
		t.Errorf("status=%v delivered=%q, want delivered %q", out["status"], cache.Delivered(), want)
	}
	out, cache = multiSinkRun(t, "b-1")
	want = "## a-1\n\nA FINDINGS\n\n## b-1\n\n" + stream.StoppedSinkNote
	if out["status"] != "delivered" || cache.Delivered() != want {
		t.Errorf("one stopped: status=%v delivered=%q, want %q", out["status"], cache.Delivered(), want)
	}
	out, cache = multiSinkRun(t, "a-1", "b-1")
	if out["status"] != "stopped" || cache.Delivered() != "" {
		t.Errorf("all stopped: status=%v delivered=%q, want stopped with no answer", out["status"], cache.Delivered())
	}
}

// TestExecuteTool_ExtensionDeliversItsOwnSinks: a turn extending a delivered plan with two new
// independent sinks delivers exactly those two, never the earlier turn's sink.
func TestExecuteTool_ExtensionDeliversItsOwnSinks(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "s-1", Task: "synthesize", TaskID: "t-s", Result: "TURN-1 TABLE"},
			{NodeID: "w-4", Task: "look it up"}, {NodeID: "w-5", Task: "look that up"},
		},
		Delivery: &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "s-1", Agent: "synthesizer"}, {NodeID: "w-4", Agent: "web-researcher"}, {NodeID: "w-5", Agent: "web-researcher"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"w-4": "W4", "w-5": "W5"}}
	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, answerOf, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.(runnableTool).Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatal(err)
	}
	if got, want := cache.Delivered(), "## w-4\n\nW4\n\n## w-5\n\nW5"; got != want {
		t.Errorf("delivered %q, want %q", got, want)
	}
	if saved, _, _ := loadDagPlan(context.Background(), c); !slices.Equal(saved.Sinks, []string{"w-4", "w-5"}) {
		t.Errorf("recorded sinks = %v, want the step's own [w-4 w-5] for a later retry or resume", saved.Sinks)
	}
}

// A delivering step in which nothing reached running has no answer, so the plan is not delivered.
func TestExecuteTool_NoSinkRanIsNotDelivered(t *testing.T) {
	rec := dag.DagPlanRecord{PlanID: "p1", Assignments: []dag.Assignment{{NodeID: "a-1", Task: "a"}}, Delivery: &dag.Delivery{Kind: "comment"}}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "a-1", Agent: "web-researcher"}})
	step := &fakeRunStep{notStarted: map[string]bool{"a-1": true}}
	tl, err := NewExecuteTool(planner, c, NewPlanCache(), nil, step.run, answerOf, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.(runnableTool).Run(newExecToolCtx(), map[string]any{"plan_id": "p1"})
	if err != nil || out["status"] == "delivered" {
		t.Errorf("status = %v err=%v, want no delivery with no sink run", out["status"], err)
	}
}

// TestExecuteTool_StoppedSeedIsFlaggedUnreviewed: a later step seeding a dependent from a
// stopped assignment's draft flags that seed as unreviewed for the step.
func TestExecuteTool_StoppedSeedIsFlaggedUnreviewed(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "a-1", Task: "a", TaskID: "t-a", Result: "STOPPED DRAFT", Stopped: true},
			{NodeID: "b-1", Task: "b", DependsOn: []string{"a-1"}},
		},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "a-1", Agent: "web-researcher"}, {NodeID: "b-1", Agent: "web-researcher"}})
	var flags map[string]bool
	run := func(ctx context.Context, _ dag.Plan, _ map[string]string, run map[string]bool) (map[string]string, map[string]bool, map[string]bool, error) {
		flags = dag.UnreviewedSeedsFrom(ctx)
		return map[string]string{"b-1": "B"}, nil, run, nil
	}
	tl, err := NewExecuteTool(planner, c, NewPlanCache(), nil, run, nil, nil, "q", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.(runnableTool).Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatal(err)
	}
	if !flags["a-1"] {
		t.Errorf("seed flags = %v, want a-1 flagged unreviewed", flags)
	}
}
