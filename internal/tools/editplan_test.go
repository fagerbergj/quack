package tools

import (
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
	createTl, err := NewCreatePlanTool(c, "orchestrator", nil, nodeIsRunning)
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

	editTl, err := NewEditPlanTool(c, "orchestrator", nil, nodeIsRunning)
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
