package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// resumeTestPlan seeds a one-node plan naming a TERMINAL (done) node id, the
// shape a reuse assignment produces, plus the dag_node record it resumes.
func resumeTestPlan(t *testing.T) (*recordstore.Client, *PlanCache) {
	t.Helper()
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl-1", Task: "keep going"}},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "impl-1", Agent: "code-implementer", Status: dag.StatusDone, ContextID: "prior-acp-session"},
	})
	return c, NewPlanCache()
}

// TestExecuteTool_ResumesTerminalNode: an assignment naming an already-done
// node id must reach the assembled plan with ResumedFrom set to that node's
// stored context id - the seed for graph.go's AdvisorTask.ACPSessionID.
func TestExecuteTool_ResumesTerminalNode(t *testing.T) {
	c, cache := resumeTestPlan(t)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	tl, err := NewExecuteTool(planner, c, cache, nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	got, ok := cache.Get("p1")
	if !ok {
		t.Fatal("plan not found in cache")
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ResumedFrom != "prior-acp-session" {
		t.Fatalf("plan.Nodes = %+v, want impl-1 with ResumedFrom=prior-acp-session", got.Nodes)
	}
}

// TestExecuteTool_FreshnessCheckClearsStaleResume: a freshness check that
// reports stale must run the same node id on a brand-new session instead -
// ResumedFrom must NOT reach the assembled plan.
func TestExecuteTool_FreshnessCheckClearsStaleResume(t *testing.T) {
	c, cache := resumeTestPlan(t)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	var sawNodeID string
	freshness := AssignmentFreshnessFunc(func(_ agent.Context, a dag.Assignment) (bool, string) {
		sawNodeID = a.NodeID
		return false, "branch moved past the node's cloned base_sha"
	})
	tl, err := NewExecuteTool(planner, c, cache, nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", freshness)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if sawNodeID != "impl-1" {
		t.Fatalf("freshness check never consulted for impl-1 (saw %q)", sawNodeID)
	}
	got, ok := cache.Get("p1")
	if !ok {
		t.Fatal("plan not found in cache")
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ResumedFrom != "" {
		t.Fatalf("plan.Nodes = %+v, want ResumedFrom cleared once freshnessCheck reports stale", got.Nodes)
	}
}

// TestExecuteTool_StampsTaskIDAndRunningStatus: execute must record a
// task_id per dispatched assignment and advance the plan record's status
// past "planned".
func TestExecuteTool_StampsTaskIDAndRunningStatus(t *testing.T) {
	c, cache := resumeTestPlan(t)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	tl, err := NewExecuteTool(planner, c, cache, nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}

	raw, _, ok, err := c.Latest(context.Background(), "dag_plan:main")
	if err != nil || !ok {
		t.Fatalf("read back dag_plan: ok=%v err=%v", ok, err)
	}
	var rec dag.DagPlanRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != "running" {
		t.Errorf("dag_plan.status = %q, want %q after execute", rec.Status, "running")
	}
	if len(rec.Assignments) != 1 || rec.Assignments[0].TaskID == "" {
		t.Errorf("assignments = %+v, want a non-empty task_id after execute dispatches", rec.Assignments)
	}
}

// TestExecuteTool_RejectionSavesAnchoredJudgeRound: a plan-judge rejection
// must both fail the tool call AND persist a judge_round record anchored to
// the authoring lineage node id, so the artifact panel shows it.
func TestExecuteTool_RejectionSavesAnchoredJudgeRound(t *testing.T) {
	rejectingJudge := vetting.PlanJudge(func(_ context.Context, _, _, _ string) (bool, string, error) {
		return false, "the plan skips required review", nil
	})
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, rejectingJudge)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl-1", Task: "keep going"}},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{{NodeID: "impl-1", Agent: "code-implementer"}})
	cache := NewPlanCache()

	tl, err := NewExecuteTool(planner, c, cache, nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err == nil {
		t.Fatal("want an error from a plan-judge rejection, got nil")
	}

	summaries, err := c.List(context.Background(), "judge_round")
	if err != nil {
		t.Fatalf("list judge_round: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("judge_round records = %d, want exactly 1 for the rejection", len(summaries))
	}
	// The hint-derived id ("<turn>-<node>-<round>") embeds the anchor node id
	// directly - lineage.NodeID round-trips only through a metaSaver-capable
	// artifact.Service, which the in-memory test double here isn't.
	if !strings.Contains(summaries[0].ID, "-orchestrator-0") {
		t.Errorf("judge_round id = %q, want it anchored to the authoring lineage id %q", summaries[0].ID, "orchestrator")
	}
	raw, _, ok, err := c.Latest(context.Background(), summaries[0].ID)
	if err != nil || !ok {
		t.Fatalf("read back judge_round: ok=%v err=%v", ok, err)
	}
	var jr vetting.JudgeRoundRecord
	if err := json.Unmarshal(raw, &jr); err != nil {
		t.Fatal(err)
	}
	if jr.Passed {
		t.Error("judge_round.Passed = true, want false for a rejection")
	}
	if len(jr.Criteria) != 1 || jr.Criteria[0].Feedback != "the plan skips required review" {
		t.Errorf("judge_round.Criteria = %+v, want the plan judge's own rejection reason", jr.Criteria)
	}
}
