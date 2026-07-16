package vetting

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
)

// stubPlanJudgeModel is a scripted model.LLM that always calls
// submit_plan_verdict with the canned accept/reason — proving NewPlanJudge's
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

// noVerdictModel ends its run without ever calling submit_plan_verdict —
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
