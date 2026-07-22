package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
)

// stubPlanJudgeModel is a scripted model.LLM that always calls
// submit_plan_verdict with the canned accept/reason - proving NewPlanJudge's
// tool wiring (submit tool built, run isolated, sink read back) without a live
// model.
type stubPlanJudgeModel struct {
	accept bool
	reason string
}

func (stubPlanJudgeModel) Name() string { return "stub-plan-judge" }

func (s stubPlanJudgeModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitPlanVerdictTool, map[string]any{"accept": s.accept, "reason": s.reason}), nil)
	}
}

// noVerdictModel ends its run without ever calling submit_plan_verdict -
// exercises NewPlanJudge's "judge ended without a verdict" error path.
type noVerdictModel struct{}

func (noVerdictModel) Name() string { return "no-verdict" }

func (noVerdictModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{TurnComplete: true}, nil)
	}
}

func TestPlanJudgeAccepts(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel{accept: true, reason: ""})
	accept, reason, err := judge(context.Background(), "write a plan", "1 node(s):\n- explore (web-researcher)")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Errorf("accept = false, want true (reason %q)", reason)
	}
}

func TestPlanJudgeRejectsWithReason(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel{accept: false, reason: "add a code-implementer node"})
	accept, reason, err := judge(context.Background(), "implement and ship it", "1 node(s):\n- explore (web-researcher)")
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if accept {
		t.Error("accept = true, want false")
	}
	if reason != "add a code-implementer node" {
		t.Errorf("reason = %q, want %q", reason, "add a code-implementer node")
	}
}

func TestPlanJudgeErrorsWithoutVerdict(t *testing.T) {
	judge := NewPlanJudge(noVerdictModel{})
	if _, _, err := judge(context.Background(), "x", "y"); err == nil {
		t.Fatal("PlanJudge: expected an error when the model never calls submit_plan_verdict")
	}
}

// TestPlanJudgeAcceptsCohesiveSingleNodePlan pins the plumbing for the case
// that motivated the criterion-7 reword: a small, cohesive change (add one
// reaction to an API) with setup + delivery declared is a ONE-node plan, and
// must be accepted rather than forced into an API/logic/tests chain.
func TestPlanJudgeAcceptsCohesiveSingleNodePlan(t *testing.T) {
	judge := NewPlanJudge(stubPlanJudgeModel{accept: true, reason: ""})
	planSummary := "1 node(s):\n" +
		"- implement (code-implementer): add a 👀 reaction to the API, implement the logic, write tests, and run checks\n" +
		"setup: repo=github.com/example/app work_branch=feat/eyes-reaction\n" +
		"delivery: kind=pull_request"
	accept, reason, err := judge(context.Background(), "add a 👀-reaction feature", planSummary)
	if err != nil {
		t.Fatalf("PlanJudge: %v", err)
	}
	if !accept {
		t.Errorf("accept = false, want true for a cohesive single-node plan (reason %q)", reason)
	}
}

// TestPlanRubricCriterion7RequiresSingleNodeForCohesiveWork pins the reworded
// rubric text against the live over-decomposition bug: the judge rejected a
// correct single-node plan for splitting "into separate nodes for API
// implementation, logic implementation, and testing/verification" - activity
// slicing, not independent-portion slicing. This asserts the instruction text
// itself so the guidance can't silently regress without a model run.
func TestPlanRubricCriterion7RequiresSingleNodeForCohesiveWork(t *testing.T) {
	mustContain := []string{
		"decompose by independent PORTION of work",
		"NEVER by activity or layer",
		"is CORRECTLY a SINGLE code-implementer node",
		"only when the request genuinely contains multiple INDEPENDENT portions",
	}
	for _, s := range mustContain {
		if !strings.Contains(planRubricInstruction, s) {
			t.Errorf("criterion 7 rubric text missing expected phrase: %q", s)
		}
	}
}

// TestPlanRubricCriterion7ForbidsActivitySplit pins that the rubric names the
// exact activity split the maintainer observed (API / logic / tests / checks
// / commit) as a FAILURE, not a pass - the bug this fix closes.
func TestPlanRubricCriterion7ForbidsActivitySplit(t *testing.T) {
	if !strings.Contains(planRubricInstruction, `splitting "API implementation" vs. "logic implementation" vs. "testing/verification" vs. "run checks" vs. "commit" into separate nodes for what is really one goal FAILS this criterion`) {
		t.Error("criterion 7 rubric text must explicitly forbid splitting one cohesive goal into API/logic/tests/checks/commit nodes")
	}
}
