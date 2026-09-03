package orchestrator

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// alwaysAcceptJudge accepts the first plan it sees - the plan tool must run
// (and save the dag_plan record) without a rejection round muddying the count.
func alwaysAcceptJudge(context.Context, string, string) (bool, string, error) { return true, "", nil }

// TestOrchestratorRun_AcceptedPlan_WritesDagPlanArtifact proves #1122: an
// orchestrator wired with SetArtifacts writes "dag_plan:main" the moment a
// plan is accepted, driven end to end through Run (not by calling
// tools.NewPlanTool directly) - the real defect was in how the artifact
// service reaches the orchestrator, not in the plan tool itself.
func TestOrchestratorRun_AcceptedPlan_WritesDagPlanArtifact(t *testing.T) {
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), // accepted first try; stub auto-executes once plan_id is in context
	}}
	o := newTestOrchWithJudge(t, stub, vetting.PlanJudge(alwaysAcceptJudge))
	svc := artifact.InMemoryService()
	o.SetArtifacts(svc)

	evs := runTurn(t, o, "research the thing")

	if hasEvent(evs, stream.EventError) {
		t.Fatalf("an accepted plan must not surface an error; events=%v", evs)
	}
	resp, err := svc.List(context.Background(), &artifact.ListRequest{AppName: AppName, UserID: "u", SessionID: "chat"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !contains(resp.FileNames, "dag_plan:main") {
		t.Fatalf("artifact file names = %v, want \"dag_plan:main\" written on plan acceptance", resp.FileNames)
	}
}

// TestOrchestratorRun_NoArtifactService_PlanStillRunsNoArtifact companions the
// above: an orchestrator that never had SetArtifacts called must still run a
// plan to completion (fail-open, same as the pre-#1095 behavior) rather than
// erroring or panicking for lack of an artifact service.
func TestOrchestratorRun_NoArtifactService_PlanStillRunsNoArtifact(t *testing.T) {
	stub := &orchStub{replies: []*model.LLMResponse{planCall()}}
	o := newTestOrchWithJudge(t, stub, vetting.PlanJudge(alwaysAcceptJudge))
	// o.SetArtifacts left uncalled deliberately.

	evs := runTurn(t, o, "research the thing")

	if hasEvent(evs, stream.EventError) {
		t.Fatalf("a plan run with no artifact service must still succeed (fail-open); events=%v", evs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer == "" {
		t.Fatalf("plan run produced no answer even though execute should have run the node")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
