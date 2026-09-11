package tools

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// newCreatePlanForTest builds a create_plan tool over a fresh in-memory
// recordstore.Client, returning both for assertions against what it persisted.
func newCreatePlanForTest(t *testing.T, roster []dag.AgentInfo, nodeIsRunning func(string) bool) (runnableTool, *recordstore.Client) {
	t.Helper()
	dag.NewPlanner(roster, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nodeIsRunning, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatalf("create_plan tool is not runnable")
	}
	return rt, c
}

// TestCreatePlanEmptyTaskRejected covers the contract table's "empty task" row.
func TestCreatePlanEmptyTaskRejected(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-implementer"}}, nil)
	assignments := []map[string]any{{"agent": "code-implementer", "task": ""}}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err == nil {
		t.Fatal("want an error for an empty task")
	}
}

// TestCreatePlanCycleRejected covers the contract table's "cycle" row.
func TestCreatePlanCycleRejected(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	assignments := []map[string]any{
		{"agent": "web-researcher", "task": "a", "depends_on": []string{"1"}},
		{"agent": "web-researcher", "task": "b", "depends_on": []string{"0"}},
	}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err == nil {
		t.Fatal("want an error for a dependency cycle")
	}
}

// TestCreatePlanRejectionMintsNoOrphanNodes is the BLOCKING regression test:
// a create_plan call whose dag_plan record fails validation must leave no
// dag_node records behind - list_nodes must not show a "hired" node from a
// call that never actually took effect.
func TestCreatePlanRejectionMintsNoOrphanNodes(t *testing.T) {
	rt, c := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	assignments := []map[string]any{
		{"agent": "web-researcher", "task": "a valid task"},
		{"agent": "web-researcher", "task": ""}, // invalid: empty task fails the plan
	}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err == nil {
		t.Fatal("want the call to error on the second assignment's empty task")
	}
	nodes, err := listDagNodeRecords(newFakeCtx(), c)
	if err != nil {
		t.Fatalf("listDagNodeRecords: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("dag_node records = %+v, want none - a rejected create_plan must not orphan the node it minted for the valid assignment", nodes)
	}
}

// TestCreatePlanMintedIDEcho covers the response carrying back the minted
// node_id the model must reference to depend on or reassign the node later.
func TestCreatePlanMintedIDEcho(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	assignments := []map[string]any{{"agent": "web-researcher", "task": "research the thing"}}
	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments})
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	// functiontool round-trips TResults through JSON, so nested fields land
	// as generic map[string]any/[]any, not the concrete Go struct.
	out, ok := res["assignments"].([]any)
	if !ok || len(out) != 1 {
		t.Fatalf("res[assignments] = %#v, want one assignment", res["assignments"])
	}
	entry, ok := out[0].(map[string]any)
	if !ok || entry["node_id"] != "web-researcher-1" || entry["agent"] != "web-researcher" {
		t.Errorf("assignments[0] = %+v, want the minted id web-researcher-1 echoed back with its agent", entry)
	}
}

// TestCreatePlanNodeCurrentlyRunningRejected covers reassigning a live node
// through create_plan (the same node_id path edit_plan also uses).
func TestCreatePlanNodeCurrentlyRunningRejected(t *testing.T) {
	rt, c := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-implementer"}}, func(id string) bool { return id == "impl-1" })
	if _, _, err := c.SaveStructured(newFakeCtx(), "dag_node",
		dag.DagNodeRecord{NodeID: "impl-1", Agent: "code-implementer"}, "impl-1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node: %v", err)
	}
	assignments := []map[string]any{{"node_id": "impl-1", "task": "keep going"}}
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments})
	if err == nil || !strings.Contains(err.Error(), "currently running") {
		t.Errorf("err = %v, want a currently-running rejection", err)
	}
}

// TestCreatePlanSetupRepoMismatchRejected is the QA rig regression test: a
// model-submitted setup.repo that disagrees with the trigger's own repo must
// be rejected immediately, without ever reaching the plan judge - the
// trigger's setup always overrides at execute time regardless, so a
// mismatched override is either hallucination or a stale assumption.
func TestCreatePlanSetupRepoMismatchRejected(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "qa-fixture-base"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	_, err = rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []map[string]any{{"agent": "code-reviewer", "task": "review it"}},
		"setup":       map[string]any{"repo": "https://github.com/quack-org/quack.git", "base_ref": "main", "work_branch": "main"},
	})
	if err == nil || !strings.Contains(err.Error(), "setup.repo") {
		t.Fatalf("err = %v, want a setup.repo mismatch rejection", err)
	}
	nodes, err := listDagNodeRecords(newFakeCtx(), c)
	if err != nil {
		t.Fatalf("listDagNodeRecords: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("dag_node records = %+v, want none - the rejection must happen before any node is minted", nodes)
	}
}

// TestCreatePlanSetupBaseRefMatchingTriggerAccepted covers the non-mismatch
// path: an explicit setup that matches the trigger exactly is not rejected.
func TestCreatePlanSetupBaseRefMatchingTriggerAccepted(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "qa-fixture-base"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	_, err = rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []map[string]any{{"agent": "code-reviewer", "task": "review it"}},
		"setup":       map[string]any{"repo": githubSetup.Repo, "base_ref": githubSetup.BaseRef, "work_branch": "quack/pr-1"},
	})
	if err != nil {
		t.Fatalf("create_plan Run: %v, want the matching setup accepted", err)
	}
}

// TestCreatePlanWorkdirEscapeRejected covers an assignment's workdir walking
// outside the workspace.
func TestCreatePlanWorkdirEscapeRejected(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-implementer"}}, nil)
	assignments := []map[string]any{{"agent": "code-implementer", "task": "x", "workdir": "../../etc"}}
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments})
	if err == nil || !strings.Contains(err.Error(), "workdir") {
		t.Errorf("err = %v, want a workdir rejection", err)
	}
}

// TestCreatePlanStampsAssignmentMetaOnGitHubTrigger: onAssignment stamps its
// return under assignment.meta.<key> - extension-owned, never model-authored.
func TestCreatePlanStampsAssignmentMetaOnGitHubTrigger(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	onAssignment := func(_ agent.Context, _, _ string, a dag.Assignment) (string, map[string]any) {
		return "github", map[string]any{"base_sha": "deadbeef"}
	}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, onAssignment)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	assignments := []map[string]any{{"agent": "code-implementer", "task": "keep going"}}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}

	rec, _, ok, err := loadDagPlan(context.Background(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if len(rec.Assignments) != 1 {
		t.Fatalf("assignments = %+v, want exactly 1", rec.Assignments)
	}
	if got := rec.Assignments[0].Meta["github"]["base_sha"]; got != "deadbeef" {
		t.Errorf("assignment.meta.github.base_sha = %v, want %q", got, "deadbeef")
	}
}

// TestCreatePlanMetaHookRunsRegardlessOfTrigger: onAssignment is generic
// over whichever extension implements AssignmentMetaExtension (like
// AssignmentFreshnessFunc, it isn't gated on a GitHub trigger) - it still
// runs on a plain chat dispatch, keyed by whatever name the hook itself returns.
func TestCreatePlanMetaHookRunsRegardlessOfTrigger(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	onAssignment := func(_ agent.Context, _, _ string, a dag.Assignment) (string, map[string]any) {
		return "acme", map[string]any{"ticket": "ACME-42"}
	}
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, onAssignment)
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	assignments := []map[string]any{{"agent": "code-implementer", "task": "keep going"}}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}

	rec, _, ok, err := loadDagPlan(context.Background(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if got := rec.Assignments[0].Meta["acme"]["ticket"]; got != "ACME-42" {
		t.Errorf("assignment.meta.acme.ticket = %v, want %q (a second, non-GitHub extension's own key)", got, "ACME-42")
	}
}
