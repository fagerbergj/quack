package tools

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// newEditPlanForTest seeds a fresh chat with one create_plan call, then
// returns the edit_plan tool over the same store plus the plan_id to edit.
func newEditPlanForTest(t *testing.T, roster []dag.AgentInfo, nodeIsRunning func(string) bool) (runnableTool, *recordstore.Client, string) {
	t.Helper()
	dag.NewPlanner(roster, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	createTl, err := NewCreatePlanTool(c, "orchestrator", nil, nodeIsRunning, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	crt := createTl.(runnableTool)
	res, err := crt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []map[string]any{{"agent": roster[0].Name, "task": "the first assignment"}},
	})
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	planID, _ := res["plan_id"].(string)

	editTl, err := NewEditPlanTool(c, "orchestrator", nil, nodeIsRunning, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	ert, ok := editTl.(runnableTool)
	if !ok {
		t.Fatalf("edit_plan tool is not runnable")
	}
	return ert, c, planID
}

// TestEditPlanUnknownPlanIDRejected covers a stale plan_id.
func TestEditPlanUnknownPlanIDRejected(t *testing.T) {
	rt, _, _ := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"plan_id": "not-the-real-plan"})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("err = %v, want a stale plan_id rejection", err)
	}
}

// TestEditPlanRemoveUnknownNodeIDRejected is the BLOCKING regression test:
// removing a node_id not on the plan must error, not silently no-op.
func TestEditPlanRemoveUnknownNodeIDRejected(t *testing.T) {
	rt, _, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"plan_id": planID, "remove": []string{"ghost-1"}})
	if err == nil {
		t.Fatal("want an error removing a node_id not on the plan")
	}
	if !strings.Contains(err.Error(), "ghost-1") {
		t.Errorf("err = %v, want it to name the unknown id", err)
	}
}

// TestEditPlanRemoveAlreadyRanAssignmentRejected pins #slice3: an
// assignment that already ran (task_id set) is load-bearing history -
// execute's seed map for any dependent added later - so removing it is a
// one-line error, not a silent rewrite.
func TestEditPlanRemoveAlreadyRanAssignmentRejected(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	rec, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	rec.Assignments[0].TaskID = "already-dispatched"
	rec.Assignments[0].Result = "the result"
	if _, _, err := c.SaveStructured(newFakeCtx(), "dag_plan", rec, "", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed already-ran assignment: %v", err)
	}

	_, err = rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"plan_id": planID, "remove": []string{"web-researcher-1"}})
	if err == nil {
		t.Fatal("want an error removing an assignment that already ran")
	}
	if !strings.Contains(err.Error(), "already ran") {
		t.Errorf("err = %v, want it to say the assignment already ran", err)
	}
}

// seedDonePlan marks the chat's current plan "done" (delivered) in place.
func seedDonePlan(t *testing.T, c *recordstore.Client) dag.DagPlanRecord {
	t.Helper()
	rec, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	rec.Status = "done"
	if _, _, err := c.SaveStructured(newFakeCtx(), "dag_plan", rec, "", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed done plan: %v", err)
	}
	return rec
}

// TestEditPlanOnDeliveredPlanStartsNewPlan is the rig regression (audit
// finding 4): a done plan rejected edit_plan outright, and the quantized 9B
// looped create_plan against the repeat guard instead of recovering. edit_plan
// on a delivered plan must instead start a fresh plan from the given
// assignments and say so plainly, plan_id included.
func TestEditPlanOnDeliveredPlanStartsNewPlan(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	seedDonePlan(t, c)

	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     planID,
		"assignments": []map[string]any{{"agent": "web-researcher", "task": "more work"}},
	})
	if err != nil {
		t.Fatalf("edit_plan Run on a delivered plan: %v, want it to start a new plan instead of erroring", err)
	}
	newPlanID, _ := res["plan_id"].(string)
	if newPlanID == "" || newPlanID == planID {
		t.Errorf("plan_id = %q, want a fresh plan_id distinct from the delivered %q", newPlanID, planID)
	}
	summary, _ := res["summary"].(string)
	if !strings.Contains(summary, "already delivered") || !strings.Contains(summary, newPlanID) {
		t.Errorf("summary = %q, want it to say plainly that a new plan started, naming its plan_id", summary)
	}

	rec, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if rec.PlanID != newPlanID || rec.Status != "planned" {
		t.Errorf("current plan = %+v, want the new plan (status planned) now current", rec)
	}
	if len(rec.Assignments) != 1 || rec.Assignments[0].Task != "more work" {
		t.Errorf("assignments = %+v, want just the new plan's one assignment", rec.Assignments)
	}
}

// TestEditPlanOnDeliveredPlanReusesExistingNode covers a follow-up that
// names a node_id from the delivered plan: dag_node records outlive a plan
// (list_nodes lists every one in the chat), so the new plan must still
// reuse it rather than mint a redundant node.
func TestEditPlanOnDeliveredPlanReusesExistingNode(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	seedDonePlan(t, c)

	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     planID,
		"assignments": []map[string]any{{"node_id": "web-researcher-1", "task": "follow-up work"}},
	})
	if err != nil {
		t.Fatalf("edit_plan Run: %v", err)
	}
	nodes, err := listDagNodeRecords(newFakeCtx(), c)
	if err != nil {
		t.Fatalf("listDagNodeRecords: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("dag_node records = %+v, want the same one node reused, not a second minted", nodes)
	}
	out, _ := res["assignments"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["node_id"] != "web-researcher-1" {
		t.Errorf("assignments = %#v, want web-researcher-1 reused", res["assignments"])
	}
}

// TestEditPlanOnDeliveredPlanWithNoAssignmentsRejected: a done plan can't
// start a new one without assignments to carry - the generic "nothing to
// change" message names the wrong remedy (no plan is being changed).
func TestEditPlanOnDeliveredPlanWithNoAssignmentsRejected(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	seedDonePlan(t, c)

	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"plan_id": planID})
	if err == nil || !strings.Contains(err.Error(), "already delivered") {
		t.Errorf("err = %v, want it to say the plan already delivered and assignments are needed", err)
	}
}

// TestEditPlanOnDeliveredPlanRemoveWithoutAssignmentsRejected: remove alone
// targets the delivered plan's own assignments, which no longer exist once
// a new plan starts - reject it by name instead of silently ignoring it.
func TestEditPlanOnDeliveredPlanRemoveWithoutAssignmentsRejected(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	seedDonePlan(t, c)

	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id": planID,
		"remove":  []string{"web-researcher-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "already delivered") {
		t.Errorf("err = %v, want it to reject remove-with-no-assignments on an already-delivered plan", err)
	}
}

// TestEditPlanOnDeliveredPlanWithAssignmentsIgnoresRemove: once assignments
// are given, a done plan starts a new one from them regardless of a stray
// `remove` - the delivered-plan wording must never mask a call that DID
// give assignments (audit finding 4 rig follow-up: it was masking the real
// missing-agent/node_id error the model needed to see).
func TestEditPlanOnDeliveredPlanWithAssignmentsIgnoresRemove(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	seedDonePlan(t, c)

	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     planID,
		"remove":      []string{"web-researcher-1"},
		"assignments": []map[string]any{{"agent": "web-researcher", "task": "more work"}},
	})
	if err != nil {
		t.Fatalf("edit_plan Run: %v, want a new plan started despite the stray remove", err)
	}
	if newPlanID, _ := res["plan_id"].(string); newPlanID == "" || newPlanID == planID {
		t.Errorf("plan_id = %q, want a fresh plan_id", newPlanID)
	}
}

// TestEditPlanOnDeliveredPlanSurfacesMissingAgentError is the rig blocker
// (audit finding 4 follow-up): a real edit_plan call on a delivered plan
// whose assignment omits both agent and node_id must return newPlanRecord's
// own error (naming the assignment index and the accepted agent names),
// never the generic delivered-plan wording - the model can't recover from
// advice that contradicts what it actually sent.
func TestEditPlanOnDeliveredPlanSurfacesMissingAgentError(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	seedDonePlan(t, c)

	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     planID,
		"assignments": []map[string]any{{"task": "some follow-up work"}},
	})
	if err == nil {
		t.Fatal("want an error for an assignment with neither agent nor node_id")
	}
	if strings.Contains(err.Error(), "already delivered") {
		t.Errorf("err = %v, want newPlanRecord's own error, not the delivered-plan wording", err)
	}
	if !strings.Contains(err.Error(), "assignments[0]") || !strings.Contains(err.Error(), "web-researcher") {
		t.Errorf("err = %v, want it to name the offending index and the accepted agent names, exactly as create_plan's does", err)
	}
}

// TestEditPlanOnDeliveredPlanSurfacesEachNewPlanRecordError checks every
// other newPlanRecord failure mode reaches the model unmasked too: unknown
// agent, unknown depends_on id, a dependency cycle, an empty task, and a
// disallowed deliverable each keep their own message on the done-plan path.
func TestEditPlanOnDeliveredPlanSurfacesEachNewPlanRecordError(t *testing.T) {
	roster := []dag.AgentInfo{{Name: "web-researcher"}, {Name: "code-reviewer"}}
	cases := []struct {
		name        string
		assignments []map[string]any
		allowedKind string
		want        string
	}{
		{
			name:        "unknown agent",
			assignments: []map[string]any{{"agent": "ghost-agent", "task": "x"}},
			want:        "ghost-agent",
		},
		{
			name:        "unknown depends_on",
			assignments: []map[string]any{{"agent": "web-researcher", "task": "x", "depends_on": []string{"no-such-node"}}},
			want:        "no-such-node",
		},
		{
			name: "dependency cycle",
			assignments: []map[string]any{
				{"agent": "web-researcher", "task": "x", "depends_on": []string{"1"}},
				{"agent": "web-researcher", "task": "y", "depends_on": []string{"0"}},
			},
			want: "cycle",
		},
		{
			name:        "empty task",
			assignments: []map[string]any{{"agent": "web-researcher", "task": ""}},
			want:        "task",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, c, planID := newEditPlanForTest(t, roster, nil)
			seedDonePlan(t, c)
			_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
				"plan_id":     planID,
				"assignments": tc.assignments,
			})
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "already delivered") {
				t.Errorf("err = %v, want newPlanRecord's own error, not the delivered-plan wording", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestEditPlanOnDeliveredPlanSurfacesDisallowedDeliverableError covers the
// last newPlanRecord failure mode: hiring an agent whose only deliverable
// this dispatch doesn't allow must keep its own message on the done-plan
// path too, not the delivered-plan wording.
func TestEditPlanOnDeliveredPlanSurfacesDisallowedDeliverableError(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}, {Name: "code-reviewer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	// Seed a delivered plan directly - create_plan would already reject
	// hiring code-implementer under an allowedKinds=["review"] dispatch too.
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Status:      "done",
		Assignments: []dag.Assignment{{NodeID: "code-reviewer-1", Task: "reviewed already", TaskID: "done-1"}},
	}
	if _, _, err := c.SaveStructured(newFakeCtx(), "dag_plan", rec, "", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed done plan: %v", err)
	}
	if _, _, err := c.SaveStructured(newFakeCtx(), "dag_node", dag.DagNodeRecord{NodeID: "code-reviewer-1", Agent: "code-reviewer"}, "code-reviewer-1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node: %v", err)
	}

	editTl, err := NewEditPlanTool(c, "orchestrator", nil, nil, []string{"review"}, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	rt := editTl.(runnableTool)
	_, err = rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     "p1",
		"assignments": []map[string]any{{"agent": "code-implementer", "task": "x"}},
	})
	if err == nil {
		t.Fatal("want an error hiring an agent whose only deliverable isn't allowed")
	}
	if strings.Contains(err.Error(), "already delivered") {
		t.Errorf("err = %v, want newPlanRecord's own error, not the delivered-plan wording", err)
	}
	if !strings.Contains(err.Error(), "does not allow") {
		t.Errorf("err = %v, want the disallowed-deliverable message", err)
	}
}

// TestEditPlanRejectsNoOpCall pins the rig regression (#slice3 review): a
// model that calls edit_plan with only plan_id and nothing to change (no
// assignments, remove, setup, or delivery) got a happy no-op result and
// looped it eight times against the same execute rejection. A no-op must
// error and name what the tool actually accepts.
func TestEditPlanRejectsNoOpCall(t *testing.T) {
	rt, _, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"plan_id": planID})
	if err == nil {
		t.Fatal("want an error for an edit_plan call with nothing to change")
	}
	if !strings.Contains(err.Error(), "nothing to change") || !strings.Contains(err.Error(), planID) {
		t.Errorf("err = %v, want it to say nothing to change and echo the plan_id", err)
	}
}

// TestEditPlanUpsertReplacesExistingAssignment covers reassigning a node
// list_nodes already showed instead of hiring a redundant one.
func TestEditPlanUpsertReplacesExistingAssignment(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     planID,
		"assignments": []map[string]any{{"node_id": "web-researcher-1", "task": "a revised task"}},
	})
	if err != nil {
		t.Fatalf("edit_plan Run: %v", err)
	}
	out, ok := res["assignments"].([]any)
	if !ok || len(out) != 1 {
		t.Fatalf("res[assignments] = %#v, want the single assignment updated in place, not appended", res["assignments"])
	}
	entry := out[0].(map[string]any)
	if entry["task"] != "a revised task" {
		t.Errorf("assignments[0].task = %v, want the revised task", entry["task"])
	}

	rec, _, ok, err := loadDagPlan(newFakeCtx(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if len(rec.Assignments) != 1 {
		t.Errorf("persisted assignments = %+v, want exactly 1 (upsert, not append)", rec.Assignments)
	}
}

// TestEditPlanDuplicateNodeIDInOneCallRejected is the BLOCKING regression
// test: the same node_id appearing twice in one edit_plan call must error,
// not silently collapse to the last write (mergeAssignments' map-keyed merge
// would otherwise swallow it before validateDagPlanRecord ever sees it).
func TestEditPlanDuplicateNodeIDInOneCallRejected(t *testing.T) {
	rt, _, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id": planID,
		"assignments": []map[string]any{
			{"node_id": "web-researcher-1", "task": "first"},
			{"node_id": "web-researcher-1", "task": "second"},
		},
	})
	if err == nil {
		t.Fatal("want an error for the same node_id twice in one edit_plan call")
	}
}

// TestEditPlanRejectionMintsNoOrphanNodes is the BLOCKING regression test
// for edit_plan's own copy of the validate-before-mint ordering.
func TestEditPlanRejectionMintsNoOrphanNodes(t *testing.T) {
	rt, c, planID := newEditPlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id":     planID,
		"assignments": []map[string]any{{"agent": "web-researcher", "task": ""}}, // invalid: empty task
	})
	if err == nil {
		t.Fatal("want an error for the empty task")
	}
	nodes, err := listDagNodeRecords(newFakeCtx(), c)
	if err != nil {
		t.Fatalf("listDagNodeRecords: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("dag_node records = %+v, want only the original web-researcher-1 - a rejected edit_plan must not orphan a freshly hired node", nodes)
	}
}

// TestEditPlanTriggerBackedIgnoresSubmittedSetupWithNote mirrors
// create_plan's own regression test (#slice3 review): edit_plan must accept
// a setup override that disagrees with the trigger's own repo, ignore it
// (the trigger's setup always overwrites rec.Setup regardless), and say so
// in the summary - not reject the whole call over a field it can't change.
func TestEditPlanTriggerBackedIgnoresSubmittedSetupWithNote(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	createTl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	crt := createTl.(runnableTool)
	res, err := crt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []map[string]any{{"agent": "web-researcher", "task": "x"}},
	})
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	planID, _ := res["plan_id"].(string)

	editTl, err := NewEditPlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewEditPlanTool: %v", err)
	}
	ert := editTl.(runnableTool)
	editRes, err := ert.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"plan_id": planID,
		"setup":   map[string]any{"repo": "https://github.com/quack-org/quack.git", "base_ref": "main", "work_branch": "main"},
	})
	if err != nil {
		t.Fatalf("edit_plan Run: %v, want the wrong setup ignored, not rejected", err)
	}
	summary, _ := editRes["summary"].(string)
	if !strings.Contains(summary, "setup ignored") || !strings.Contains(summary, githubSetup.Repo) {
		t.Errorf("summary = %q, want a setup-ignored note naming the trigger's own repo", summary)
	}
	rec, _, ok, err := loadDagPlan(context.Background(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if rec.Setup == nil || rec.Setup.Repo != githubSetup.Repo {
		t.Errorf("rec.Setup = %+v, want the trigger's own setup, not the model's guess", rec.Setup)
	}
}
