// execute_incremental_test.go: pins slice-3 incremental planning at the
// execute tool level - a partial step (no delivery) returns results and
// leaves the turn open, a second execute call dispatches only what hasn't
// run yet, and a delivering step ends the turn and sets the cache's answer.
package tools

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

// fakeRunStep records the run/seeded sets it was called with and returns
// canned outputs, keyed by node id.
type fakeRunStep struct {
	calls      int
	lastRun    map[string]bool
	lastSeeded map[string]string
	outputs    map[string]string
	paused     bool
}

func (f *fakeRunStep) run(_ context.Context, _ dag.Plan, seeded map[string]string, run map[string]bool) (map[string]string, bool, error) {
	f.calls++
	f.lastRun = run
	f.lastSeeded = seeded
	out := make(map[string]string, len(run))
	for id := range run {
		if v, ok := f.outputs[id]; ok {
			out[id] = v
		}
	}
	return out, f.paused, nil
}

// execResults reads out["results"] (a []any of map[string]any, the shape a
// functiontool.Run's JSON round-trip leaves executeResult in) as the single
// entry a single-assignment test expects.
func execResult(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list, ok := out["results"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("out[results] = %#v, want exactly one entry", out["results"])
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("out[results][0] = %#v, want an object", list[0])
	}
	return entry
}

// TestExecuteTool_PartialStepReturnsResultsAndContinuesTurn: a plan with no
// declared delivery is a partial step - execute runs it, returns per-node
// results, and must NOT set SkipSummarization (the model's turn continues).
func TestExecuteTool_PartialStepReturnsResultsAndContinuesTurn(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "r-1", Task: "find the file"}},
	}
	// NewPlanner registers the agent roster dag_node records validate against
	// (dag.SetAgentRoster, package-global) - must run before seedPlanRecord.
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "r-1", Agent: "web-researcher"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"r-1": "FOUND: file.go defines it"}}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "find the file", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if got := out["status"]; got != "running" {
		t.Errorf("status = %v, want %q (no delivery declared)", got, "running")
	}
	entry := execResult(t, out)
	if entry["node_id"] != "r-1" || entry["status"] != "done" {
		t.Fatalf("results[0] = %+v, want a done result for r-1", entry)
	}
	if s, _ := entry["task_id"].(string); s == "" {
		t.Error("results[0].task_id is empty, want a minted task_id")
	}
	if s, _ := entry["summary"].(string); s == "" {
		t.Error("results[0].summary is empty, want the node's output preview")
	}
	if ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = true on a partial (no-delivery) step, want false - the turn must continue")
	}
	if cache.Delivered() != "" {
		t.Errorf("cache.Delivered() = %q, want empty - nothing was delivered yet", cache.Delivered())
	}
}

// TestExecuteTool_PausedStepMarksNodePausedAndEndsTurn: a node that parks on
// a HITL question must be reported "paused" (not "failed"), must keep no
// task_id (so a later execute retries it fresh, not seeds it with an empty
// result forever), and must still end the orchestrator's turn - looping the
// model straight back into execute/edit_plan would just repeat the step.
func TestExecuteTool_PausedStepMarksNodePausedAndEndsTurn(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl-1", Task: "ask the user something"}},
	}
	// NewPlanner registers the agent roster dag_node records validate against
	// (dag.SetAgentRoster, package-global) - must run before seedPlanRecord.
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "impl-1", Agent: "code-implementer"}})
	cache := NewPlanCache()
	step := &fakeRunStep{paused: true} // no output for impl-1 - it's waiting on the user

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "ask the user something", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if got := out["status"]; got != "paused" {
		t.Errorf("status = %v, want %q", got, "paused")
	}
	entry := execResult(t, out)
	if entry["status"] != "paused" {
		t.Fatalf("results[0] = %+v, want impl-1 reported paused, not failed", entry)
	}
	if s, _ := entry["task_id"].(string); s != "" {
		t.Errorf("results[0].task_id = %q, want empty - a paused assignment must stay eligible to run again", s)
	}
	if !ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = false on a paused step, want true - the turn must end, not loop back into execute")
	}

	rec2, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if rec2.Assignments[0].TaskID != "" {
		t.Errorf("persisted task_id = %q, want empty for a paused assignment", rec2.Assignments[0].TaskID)
	}
}

// TestExecuteTool_SecondExecuteRunsOnlyNewAssignments: an assignment that
// already ran (task_id set) must not be re-dispatched, and its result seeds
// a dependent new assignment's run.
func TestExecuteTool_SecondExecuteRunsOnlyNewAssignments(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "r-1", Task: "find the file", TaskID: "already-ran", Result: "FOUND: file.go"},
			{NodeID: "impl-1", Task: "add the comment", DependsOn: []string{"r-1"}},
		},
	}
	// NewPlanner registers the agent roster dag_node records validate against
	// (dag.SetAgentRoster, package-global) - must run before seedPlanRecord.
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-implementer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "r-1", Agent: "web-researcher", Status: dag.StatusDone, Started: true},
		{NodeID: "impl-1", Agent: "code-implementer"},
	})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"impl-1": "commented"}}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "add the comment", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}

	if step.calls != 1 {
		t.Fatalf("runStep called %d times, want 1", step.calls)
	}
	if len(step.lastRun) != 1 || !step.lastRun["impl-1"] {
		t.Errorf("run set = %+v, want only impl-1 - r-1 already ran", step.lastRun)
	}
	if step.lastSeeded["r-1"] != "FOUND: file.go" {
		t.Errorf("seeded[r-1] = %q, want the already-ran result handed to the new dependent node", step.lastSeeded["r-1"])
	}
}

// TestExecuteTool_DeliveringStepEndsTurnAndDelivers: once the plan declares
// delivery, execute ends the model's turn and records the finalized answer.
func TestExecuteTool_DeliveringStepEndsTurnAndDelivers(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "s-1", Task: "answer"}},
		Delivery:    &dag.Delivery{Kind: "comment"},
	}
	// NewPlanner registers the agent roster dag_node records validate against
	// (dag.SetAgentRoster, package-global) - must run before seedPlanRecord.
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "s-1", Agent: "synthesizer"}})
	cache := NewPlanCache()
	step := &fakeRunStep{outputs: map[string]string{"s-1": "FINAL ANSWER"}}
	var finalizeSaw map[string]string
	finalize := func(_ context.Context, _ dag.Plan, outputs map[string]string) string {
		finalizeSaw = outputs
		return "THE ANSWER"
	}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, finalize, nil, "answer", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if got := out["status"]; got != "delivered" {
		t.Errorf("status = %v, want %q", got, "delivered")
	}
	if !ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = false on a delivering step, want true - the turn must end")
	}
	if cache.Delivered() != "THE ANSWER" {
		t.Errorf("cache.Delivered() = %q, want %q", cache.Delivered(), "THE ANSWER")
	}
	if finalizeSaw["s-1"] != "FINAL ANSWER" {
		t.Errorf("finalize outputs[s-1] = %q, want the step's own result", finalizeSaw["s-1"])
	}
}

// TestExecuteTool_NothingNewToRunSkipsJudgeAndReturnsClearResult pins the CI
// hang fix (#slice3 review, BLOCKING 3a): a call with every assignment
// already run must not re-run the judge/build/finalize pipeline - it's a
// clear no-op result, and runStep must never be invoked.
func TestExecuteTool_NothingNewToRunSkipsJudgeAndReturnsClearResult(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "r-1", Task: "find the file", TaskID: "already-ran", Result: "FOUND"}},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "r-1", Agent: "web-researcher", Status: dag.StatusDone, Started: true}})
	cache := NewPlanCache()
	step := &fakeRunStep{}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "find the file", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if step.calls != 0 {
		t.Errorf("runStep called %d times, want 0 - nothing new must skip the judge/run pipeline entirely", step.calls)
	}
	if got := out["status"]; got != "running" {
		t.Errorf("status = %v, want %q (plan not yet done)", got, "running")
	}
	if msg, _ := out["message"].(string); msg == "" {
		t.Error("message is empty, want a clear \"nothing new to run\" explanation")
	}
	if ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = true for an undone plan with nothing new, want false - the model may still edit_plan/declare delivery")
	}
}

// TestExecuteTool_NothingNewOnDonePlanEndsTurn: the same no-op short-circuit,
// but the plan already delivered - repeating the call must not leave the
// turn hanging open forever.
func TestExecuteTool_NothingNewOnDonePlanEndsTurn(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "s-1", Task: "answer", TaskID: "already-ran", Result: "DONE"}},
		Delivery:    &dag.Delivery{Kind: "comment"},
		Status:      "done",
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "s-1", Agent: "synthesizer", Status: dag.StatusDone, Started: true}})
	cache := NewPlanCache()
	step := &fakeRunStep{}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "answer", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if step.calls != 0 {
		t.Errorf("runStep called %d times, want 0", step.calls)
	}
	if got := out["status"]; got != "delivered" {
		t.Errorf("status = %v, want %q", got, "delivered")
	}
	if !ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = false on an already-done plan, want true - repeating this call must not spin the turn")
	}
}

// TestExecuteTool_FailedDeliveringStepDoesNotFinalizeOrEndTurn pins BLOCKING
// 3b: a delivering step whose node actually failed must not be marked done
// or finalized on garbage - the turn stays open so the model can react.
func TestExecuteTool_FailedDeliveringStepDoesNotFinalizeOrEndTurn(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "s-1", Task: "answer"}},
		Delivery:    &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "synthesizer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "s-1", Agent: "synthesizer"}})
	cache := NewPlanCache()
	step := &fakeRunStep{} // no output for s-1 - it failed
	finalizeCalled := false
	finalize := func(_ context.Context, _ dag.Plan, _ map[string]string) string {
		finalizeCalled = true
		return "SHOULD NOT HAPPEN"
	}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, finalize, nil, "answer", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if got := out["status"]; got != "running" {
		t.Errorf("status = %v, want %q - a failed delivering step must not report delivered", got, "running")
	}
	entry := execResult(t, out)
	if entry["status"] != "failed" {
		t.Errorf("results[0].status = %v, want %q", entry["status"], "failed")
	}
	if finalizeCalled {
		t.Error("finalize was called on a failed delivering step - must not finalize on garbage")
	}
	if cache.Delivered() != "" {
		t.Errorf("cache.Delivered() = %q, want empty", cache.Delivered())
	}
	if ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = true on a failed delivering step, want false - the turn must stay open so the model can react")
	}

	rec2, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if rec2.Status == "done" {
		t.Error("persisted dag_plan status = done, want it to stay undone after a failed delivering step")
	}
}
