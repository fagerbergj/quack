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
	needsInput map[string]bool
	notStarted map[string]bool // run-set node ids that never reached running (a dependency paused first, #slice3 review)
}

func (f *fakeRunStep) run(_ context.Context, _ dag.Plan, seeded map[string]string, run map[string]bool) (map[string]string, map[string]bool, map[string]bool, error) {
	f.calls++
	f.lastRun = run
	f.lastSeeded = seeded
	out := make(map[string]string, len(run))
	started := make(map[string]bool, len(run))
	for id := range run {
		if f.notStarted[id] {
			continue
		}
		started[id] = true
		if v, ok := f.outputs[id]; ok {
			out[id] = v
		}
	}
	return out, f.needsInput, started, nil
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
	step := &fakeRunStep{needsInput: map[string]bool{"impl-1": true}} // no output for impl-1 - it's waiting on the user

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

// TestExecuteTool_MixedPausedAndFailedReportsEachCorrectly pins the
// reviewer's finding (#slice3 review): ApplyAssignmentOutcome used to take
// the whole step's aggregate "did anything pause" flag, so a genuinely
// failed node (empty output, nothing to do with a question) sharing a step
// with a paused one was reported "paused" too - misleading, and it means
// the model doesn't learn a real failure happened. Each node's own status
// must come from whether THAT node itself paused, not the step overall.
func TestExecuteTool_MixedPausedAndFailedReportsEachCorrectly(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "ask-1", Task: "ask the user something"},
			{NodeID: "impl-1", Task: "do the thing"},
		},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "ask-1", Agent: "code-implementer"},
		{NodeID: "impl-1", Agent: "code-implementer"},
	})
	cache := NewPlanCache()
	// ask-1 parks on a question (no output, needsInput); impl-1 genuinely
	// fails (no output, NOT in needsInput) - same step, same empty output,
	// different reasons.
	step := &fakeRunStep{needsInput: map[string]bool{"ask-1": true}}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, nil, nil, "do it", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	out, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	list, ok := out["results"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("out[results] = %#v, want exactly two entries", out["results"])
	}
	statuses := map[string]string{}
	for _, e := range list {
		entry := e.(map[string]any)
		statuses[entry["node_id"].(string)] = entry["status"].(string)
	}
	if statuses["ask-1"] != "paused" {
		t.Errorf("ask-1 status = %q, want paused", statuses["ask-1"])
	}
	if statuses["impl-1"] != "failed" {
		t.Errorf("impl-1 status = %q, want failed (it never asked a question - reporting it paused would be misleading)", statuses["impl-1"])
	}

	rec2, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	byID := map[string]dag.Assignment{}
	for _, a := range rec2.Assignments {
		byID[a.NodeID] = a
	}
	if byID["ask-1"].TaskID != "" {
		t.Errorf("ask-1 persisted task_id = %q, want empty - a paused assignment must stay eligible to run again", byID["ask-1"].TaskID)
	}
	if byID["impl-1"].TaskID == "" {
		t.Error("impl-1 persisted task_id is empty, want a minted one - a failed assignment still gets one (execute.go's own design)")
	}
}

// TestExecuteTool_NeverStartedDependentStaysQueuedNotFailed pins the second
// #slice3 review's blocking regression: a node whose dependency pauses first
// in the same runSubset never gets its own goroutine dispatched (topo layers
// abort on the first error), so runStep reports it in neither outputs nor
// needsInput. Marking that "failed" (as ApplyAssignmentOutcome's default
// branch used to, unconditionally) mints a task_id that editplan.go then
// refuses to ever reassign - the plan is stuck forever. It must instead be
// left exactly as before the call: empty task_id, reported "queued".
func TestExecuteTool_NeverStartedDependentStaysQueuedNotFailed(t *testing.T) {
	rec := dag.DagPlanRecord{
		PlanID: "p1",
		Assignments: []dag.Assignment{
			{NodeID: "ask-1", Task: "ask the direction"},
			{NodeID: "close-1", Task: "close it out", DependsOn: []string{"ask-1"}},
		},
		Delivery: &dag.Delivery{Kind: "comment"},
	}
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "asker"}, {Name: "closer"}}, nil, nil)
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "ask-1", Agent: "asker"},
		{NodeID: "close-1", Agent: "closer"},
	})
	cache := NewPlanCache()
	// ask-1 pauses; close-1 is in the run set but never reaches running - the
	// exact shape runDAGSubset produces when an earlier layer's node parks.
	step := &fakeRunStep{needsInput: map[string]bool{"ask-1": true}, notStarted: map[string]bool{"close-1": true}}
	finalizeCalled := false
	finalize := func(_ context.Context, _ dag.Plan, _ map[string]string) string {
		finalizeCalled = true
		return "SHOULD NOT HAPPEN"
	}

	tl, err := NewExecuteTool(planner, c, cache, nil, step.run, finalize, nil, "ask the direction", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	ctx := newExecToolCtx()
	out, err := rt.Run(ctx, map[string]any{"plan_id": "p1"})
	if err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	list, ok := out["results"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("out[results] = %#v, want exactly two entries", out["results"])
	}
	statuses := map[string]string{}
	for _, e := range list {
		entry := e.(map[string]any)
		statuses[entry["node_id"].(string)] = entry["status"].(string)
	}
	if statuses["ask-1"] != "paused" {
		t.Errorf("ask-1 status = %q, want paused", statuses["ask-1"])
	}
	if statuses["close-1"] != "queued" {
		t.Errorf("close-1 status = %q, want queued - it never started, and must not be marked failed", statuses["close-1"])
	}
	if finalizeCalled {
		t.Error("finalize was called though the delivering node never ran")
	}

	rec2, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	byID := map[string]dag.Assignment{}
	for _, a := range rec2.Assignments {
		byID[a.NodeID] = a
	}
	if byID["close-1"].TaskID != "" {
		t.Errorf("close-1 persisted task_id = %q, want empty - a never-started node must stay dispatchable", byID["close-1"].TaskID)
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
