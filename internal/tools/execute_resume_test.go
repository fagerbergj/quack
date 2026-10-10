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

// resumeTestPlan seeds a one-node plan naming a done node id, plus the dag_node record it resumes.
func resumeTestPlan(t *testing.T) (*recordstore.Client, *PlanCache) {
	t.Helper()
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl-1", Task: "keep going"}},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "impl-1", Agent: "code-implementer", Status: dag.StatusDone, ContextID: "prior-acp-session", Started: true},
	})
	return c, NewPlanCache()
}

// An assignment naming a done node id gets ResumedFrom = that node's stored context id, which seeds
// graph.go's AdvisorTask.ACPSessionID.
func TestExecuteTool_ResumesTerminalNode(t *testing.T) {
	c, cache := resumeTestPlan(t)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	var got dag.Plan
	tl, err := NewExecuteTool(planner, c, cache, nil, capturePlan(&got), nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ResumedFrom != "prior-acp-session" {
		t.Fatalf("plan.Nodes = %+v, want impl-1 with ResumedFrom=prior-acp-session", got.Nodes)
	}
}

// An ACP node that failed after establishing a real session (Started=true) is still reused, threading that
// real id into ResumedFrom as session/load's target.
func TestExecuteTool_ResumesFailedNodeWithRealSession(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl-1", Task: "keep going"}},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "impl-1", Agent: "code-implementer", Status: dag.StatusFailed, ContextID: "acp-real-session-on-failure", Started: true},
	})
	cache := NewPlanCache()
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	var got dag.Plan
	tl, err := NewExecuteTool(planner, c, cache, nil, capturePlan(&got), nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ResumedFrom != "acp-real-session-on-failure" {
		t.Fatalf("plan.Nodes = %+v, want impl-1 resumed with the real session id", got.Nodes)
	}
}

// A node that failed before running (Started=false) has only a mint-time placeholder, never a ResumedFrom.
func TestExecuteTool_NeverStartedFailedNodeIsNotResumed(t *testing.T) {
	dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	rec := dag.DagPlanRecord{
		PlanID:      "p1",
		Assignments: []dag.Assignment{{NodeID: "impl-1", Task: "keep going"}},
	}
	c := seedPlanRecord(t, rec, []dag.DagNodeRecord{
		{NodeID: "impl-1", Agent: "code-implementer", Status: dag.StatusFailed, ContextID: "chat1:impl-1", Started: false},
	})
	cache := NewPlanCache()
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	var got dag.Plan
	tl, err := NewExecuteTool(planner, c, cache, nil, capturePlan(&got), nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
	if err != nil {
		t.Fatalf("NewExecuteTool: %v", err)
	}
	rt := tl.(runnableTool)
	if _, err := rt.Run(newExecToolCtx(), map[string]any{"plan_id": "p1"}); err != nil {
		t.Fatalf("execute Run: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ResumedFrom != "" {
		t.Fatalf("plan.Nodes = %+v, want impl-1 NOT resumed (never started)", got.Nodes)
	}
}

// A stale freshness check runs the same node id on a brand-new session: no ResumedFrom.
func TestExecuteTool_FreshnessCheckClearsStaleResume(t *testing.T) {
	c, cache := resumeTestPlan(t)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	var sawNodeID string
	freshness := AssignmentFreshnessFunc(func(_ agent.Context, _, _, _ string, a dag.Assignment) (bool, string) {
		sawNodeID = a.NodeID
		return false, "branch moved past the node's cloned base_sha"
	})
	var got dag.Plan
	tl, err := NewExecuteTool(planner, c, cache, nil, capturePlan(&got), nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", freshness)
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
	if len(got.Nodes) != 1 || got.Nodes[0].ResumedFrom != "" {
		t.Fatalf("plan.Nodes = %+v, want ResumedFrom cleared once freshnessCheck reports stale", got.Nodes)
	}
}

func TestExecuteTool_StampsTaskIDAndRunningStatus(t *testing.T) {
	c, cache := resumeTestPlan(t)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "code-implementer"}}, nil, nil)
	tl, err := NewExecuteTool(planner, c, cache, nil, nil, nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
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

// A plan-judge rejection fails the call and persists a judge_round anchored to the authoring node id.
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

	tl, err := NewExecuteTool(planner, c, cache, nil, nil, nil, nil, "keep going", nil, nil, nil, "", nil, false, "orchestrator", nil)
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
	// The hint-derived id embeds the anchor node id; lineage.NodeID needs a metaSaver-capable service,
	// which the in-memory double is not.
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
