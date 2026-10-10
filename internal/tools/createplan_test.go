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

func newCreatePlanForTest(t *testing.T, roster []dag.AgentInfo, nodeIsRunning func(string) bool) (runnableTool, *recordstore.Client) {
	t.Helper()
	dag.NewPlanner(roster, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nodeIsRunning, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt, ok := tl.(runnableTool)
	if !ok {
		t.Fatalf("create_plan tool is not runnable")
	}
	return rt, c
}

func TestCreatePlanEmptyTaskRejected(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-implementer"}}, nil)
	assignments := []map[string]any{{"agent": "code-implementer", "task": ""}}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err == nil {
		t.Fatal("want an error for an empty task")
	}
}

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

// A create_plan call whose dag_plan record fails validation must leave no dag_node records behind.
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

func TestCreatePlanMintedIDEcho(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "web-researcher"}}, nil)
	assignments := []map[string]any{{"agent": "web-researcher", "task": "research the thing"}}
	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments})
	if err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}
	// functiontool round-trips results through JSON, so nested fields land as map[string]any/[]any.
	out, ok := res["assignments"].([]any)
	if !ok || len(out) != 1 {
		t.Fatalf("res[assignments] = %#v, want one assignment", res["assignments"])
	}
	entry, ok := out[0].(map[string]any)
	if !ok || entry["node_id"] != "web-researcher-1" || entry["agent"] != "web-researcher" {
		t.Errorf("assignments[0] = %+v, want the minted id web-researcher-1 echoed back with its agent", entry)
	}
}

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

// A trigger-backed dispatch accepts a submitted setup that disagrees with the trigger's repo, ignores it
// (the trigger's setup always wins), and says so in the summary.
func TestCreatePlanTriggerBackedIgnoresSubmittedSetupWithNote(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "qa-fixture-base"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	res, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{
		"assignments": []map[string]any{{"agent": "code-reviewer", "task": "review it"}},
		"setup":       map[string]any{"repo": "https://github.com/quack-org/quack.git", "base_ref": "main", "work_branch": "main"},
	})
	if err != nil {
		t.Fatalf("create_plan Run: %v, want the wrong setup ignored, not rejected", err)
	}
	summary, _ := res["summary"].(string)
	if !strings.Contains(summary, "setup ignored") || !strings.Contains(summary, githubSetup.Repo) || !strings.Contains(summary, githubSetup.BaseRef) {
		t.Errorf("summary = %q, want a setup-ignored note naming the trigger's own repo/base_ref", summary)
	}
	rec, ok, err := loadDagPlan(context.Background(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if rec.Setup == nil || rec.Setup.Repo != githubSetup.Repo {
		t.Errorf("rec.Setup = %+v, want the trigger's own setup, not the model's guess", rec.Setup)
	}
}

func TestCreatePlanSetupBaseRefMatchingTriggerAccepted(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-reviewer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "qa-fixture-base"}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, nil, dag.AgentNamesFor(context.Background()))
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

func TestCreatePlanWorkdirEscapeRejected(t *testing.T) {
	rt, _ := newCreatePlanForTest(t, []dag.AgentInfo{{Name: "code-implementer"}}, nil)
	assignments := []map[string]any{{"agent": "code-implementer", "task": "x", "workdir": "../../etc"}}
	_, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments})
	if err == nil || !strings.Contains(err.Error(), "workdir") {
		t.Errorf("err = %v, want a workdir rejection", err)
	}
}

// onAssignment stamps its return under assignment.meta.<key>: extension-owned, never model-authored.
func TestCreatePlanStampsAssignmentMetaOnGitHubTrigger(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	githubSetup := &dag.Setup{Repo: "https://github.com/fagerbergj/quack.git", BaseRef: "main"}
	onAssignment := func(_ agent.Context, _, _ string, a dag.Assignment) (string, map[string]any) {
		return "github", map[string]any{"base_sha": "deadbeef"}
	}
	tl, err := NewCreatePlanTool(c, "orchestrator", githubSetup, nil, nil, onAssignment, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	assignments := []map[string]any{{"agent": "code-implementer", "task": "keep going"}}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}

	rec, ok, err := loadDagPlan(context.Background(), c)
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

// onAssignment is not gated on a GitHub trigger: it runs on a plain chat dispatch too, keyed by the name
// the hook returns.
func TestCreatePlanMetaHookRunsRegardlessOfTrigger(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	c := recordstore.New(artifact.InMemoryService(), "quack", "u1", "chat1")
	onAssignment := func(_ agent.Context, _, _ string, a dag.Assignment) (string, map[string]any) {
		return "acme", map[string]any{"ticket": "ACME-42"}
	}
	tl, err := NewCreatePlanTool(c, "orchestrator", nil, nil, nil, onAssignment, dag.AgentNamesFor(context.Background()))
	if err != nil {
		t.Fatalf("NewCreatePlanTool: %v", err)
	}
	rt := tl.(runnableTool)
	assignments := []map[string]any{{"agent": "code-implementer", "task": "keep going"}}
	if _, err := rt.Run(planToolCtx{newFakeCtx()}, map[string]any{"assignments": assignments}); err != nil {
		t.Fatalf("create_plan Run: %v", err)
	}

	rec, ok, err := loadDagPlan(context.Background(), c)
	if err != nil || !ok {
		t.Fatalf("loadDagPlan: ok=%v err=%v", ok, err)
	}
	if got := rec.Assignments[0].Meta["acme"]["ticket"]; got != "ACME-42" {
		t.Errorf("assignment.meta.acme.ticket = %v, want %q (a second, non-GitHub extension's own key)", got, "ACME-42")
	}
}
