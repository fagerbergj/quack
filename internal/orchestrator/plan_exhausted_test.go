package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// #693: when the plan judge rejects every plan the orchestrator proposes and
// it gives up without ever calling execute, that is a FAILED run, not an
// answer - the run must never post the judge's internal rejection text as if
// it were a reply to the user.

// rejectAlwaysJudge always rejects with reason - mimics a plan judge that
// never finds an acceptable plan.
func rejectAlwaysJudge(reason string) vetting.PlanJudge {
	return func(context.Context, string, string) (bool, string, error) {
		return false, reason, nil
	}
}

// newTestOrchWithJudge is newTestOrch (continue_test.go) with a plan judge wired in.
func newTestOrchWithJudge(t *testing.T, stub *orchStub, judge vetting.PlanJudge) *Orchestrator {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "ROLE:researcher",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions,
		map[string]adkagent.Agent{"web-researcher": worker},
		map[string]model.LLM{"web-researcher": stub},
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, judge)
	return New(sessions, stub, "You are the orchestrator.", planner, ex, nil, nil, nil)
}

// TestOrchestrator_PlanExhausted_PostsFixedNoticeNotJudgeReason: the model
// retries the plan tool against an always-rejecting judge, then gives up and
// narrates the rejection back in prose (the live NightsOut#97 symptom). The
// run must post the fixed failure notice instead, and the judge's internal
// reason text must not appear anywhere in the final answer.
func TestOrchestrator_PlanExhausted_PostsFixedNoticeNotJudgeReason(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const judgeReason = "The plan lacks a terminal node that delivers the requested artifact."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), // rejected
		planCall(), // rejected again
		// gives up and narrates the rejection reason as if it were an answer -
		// exactly what must NOT reach the user.
		stubText("I looked into this: " + judgeReason),
	}}
	o := newTestOrchWithJudge(t, stub, rejectAlwaysJudge(judgeReason))

	evs := runTurn(t, o, "address these findings")

	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if answer != planExhaustedNotice {
		t.Errorf("answer = %q, want the fixed notice %q", answer, planExhaustedNotice)
	}
	if strings.Contains(answer, "terminal node") || strings.Contains(answer, judgeReason) {
		t.Errorf("answer leaked the judge's internal rejection text: %q", answer)
	}
	if !hasEvent(evs, stream.EventError) {
		t.Errorf("a planning-exhausted run must surface an error event so the failure is evident; events=%v", evs)
	}
	if !strings.Contains(logs.String(), judgeReason) {
		t.Errorf("the judge's reason must be logged even though it's kept out of the reply; logs=%q", logs.String())
	}
}

// TestOrchestrator_PlanRejectedOnce_ThenAnswers_PivotDelivered pins #760/
// home-server#3: a reply-only request where the orchestrator over-eagerly
// tries to plan, the judge correctly rejects the plan for exceeding the
// deliverable, and the model then pivots to answering the question directly.
// One rejection is not exhaustion - the model's real answer must be delivered
// verbatim, not replaced by the fixed notice.
func TestOrchestrator_PlanRejectedOnce_ThenAnswers_PivotDelivered(t *testing.T) {
	const pivotAnswer = "The off-by-one is in the loop bound on line 42."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), // rejected: this deliverable is reply-only, not a plan
		stubText(pivotAnswer),
	}}
	o := newTestOrchWithJudge(t, stub, rejectAlwaysJudge("this deliverable is reply-only; drop the plan"))

	evs := runTurn(t, o, "why does this loop miss the last element?")

	if hasEvent(evs, stream.EventError) {
		t.Errorf("a single rejection followed by a real answer must not surface an error; events=%v", evs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != pivotAnswer {
		t.Errorf("answer = %q, want the model's own pivot answer %q delivered verbatim", answer, pivotAnswer)
	}
}

// TestOrchestrator_RejectionDoesNotLeakAcrossTurns proves PlanCache (built
// fresh per Run() call, tools.NewPlanCache in orchestrator.go) does not carry
// a rejection recorded in one turn into the next: turn one exhausts (2
// rejections, fixed notice), turn two in the SAME session answers directly
// with no plan attempt at all and must deliver normally, not be treated as
// exhausted leftover from turn one.
func TestOrchestrator_RejectionDoesNotLeakAcrossTurns(t *testing.T) {
	const turnTwoAnswer = "Turn two: a plain answer, no plan involved."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), // turn 1: rejected
		planCall(), // turn 1: rejected again -> exhausted
		stubText("turn 1 give-up narration"),
		stubText(turnTwoAnswer), // turn 2: direct answer, plan tool never called
	}}
	o := newTestOrchWithJudge(t, stub, rejectAlwaysJudge("no terminal node"))

	runTurn(t, o, "turn one: build me a plan")
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != planExhaustedNotice {
		t.Fatalf("turn 1 answer = %q, want the fixed notice %q (setup for this test)", answer, planExhaustedNotice)
	}

	evs := runTurn(t, o, "turn two: unrelated question")
	if hasEvent(evs, stream.EventError) {
		t.Errorf("turn two produced no rejection of its own; it must not inherit turn one's exhaustion; events=%v", evs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != turnTwoAnswer {
		t.Errorf("turn 2 answer = %q, want %q - a prior turn's rejections must not leak into a new turn's PlanCache", answer, turnTwoAnswer)
	}
}

// TestOrchestrator_PlanRejectedThenAccepted_NotTreatedAsExhausted: a plan
// rejected once and then accepted on retry must deliver normally - a single
// rejection along the way is not "exhausted", it's iteration working as
// intended.
func TestOrchestrator_PlanRejectedThenAccepted_NotTreatedAsExhausted(t *testing.T) {
	calls := 0
	judge := vetting.PlanJudge(func(context.Context, string, string) (bool, string, error) {
		calls++
		if calls == 1 {
			return false, "add a terminal node", nil
		}
		return true, "", nil
	})
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), // rejected
		planCall(), // accepted; stub auto-executes once plan_id is in context
	}}
	o := newTestOrchWithJudge(t, stub, judge)

	evs := runTurn(t, o, "research the thing")

	if hasEvent(evs, stream.EventError) {
		t.Errorf("a plan that's eventually accepted must not surface an error; events=%v", evs)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); !strings.Contains(answer, "RESEARCH-RESULT") {
		t.Errorf("answer = %q, want the executed node's output - recovery after one rejection must still deliver", answer)
	}
}
