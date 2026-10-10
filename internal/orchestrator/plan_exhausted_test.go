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

// rejectAlwaysJudge always rejects with reason - mimics a plan judge that
// never finds an acceptable plan.
func rejectAlwaysJudge(reason string) vetting.PlanJudge {
	return func(context.Context, string, string, string) (bool, string, error) {
		return false, reason, nil
	}
}

// planCallWidened is planCall with a second researcher: a genuinely different plan, which the
// plan loop guard must not mistake for re-proposing the rejected one.
func planCallWidened() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{
		"assignments": []any{
			map[string]any{"agent": "web-researcher", "task": "research the thing"},
			map[string]any{"agent": "web-researcher", "task": "research the other thing"},
		},
		"delivery": map[string]any{"kind": "comment"},
	})
}

// The judge rejecting the same plan twice ends the turn with a get_user_choice; answering
// "run it as is" (even wrapped) runs that plan past the judge next turn.
func TestOrchestrator_PlanLoopAsksUser(t *testing.T) {
	reason := "the user asked for no synthesizer\n" + strings.Repeat("x", 400)
	stub := &orchStub{replies: []*model.LLMResponse{planCall(), planCall(), planCall()}}
	o := newTestOrchWithJudge(t, stub, rejectAlwaysJudge(reason))

	evs := runTurn(t, o, "exactly two researchers, no synthesizer")
	if hasEvent(evs, stream.EventError) || stub.invocations() != 2 {
		t.Fatalf("orchestrator calls = %d events=%v, want two plans and no error", stub.invocations(), evs)
	}
	q, ok := o.PendingQuestion(context.Background(), "u", "chat")
	if !ok || !strings.Contains(q, "the user asked for no synthesizer "+strings.Repeat("x", 266)+"…") || strings.Contains(q, strings.Repeat("x", 267)) {
		t.Fatalf("pending question = %q %v, want the reason on one line, truncated", q, ok)
	}
	if !strings.Contains(q, planLoopRunAsIs) || !hasToolCall(evs, "get_user_choice") {
		t.Errorf("question %q / events %v, want the options named and the choice streamed", q, evs)
	}

	evs = runTurn(t, o, "<reply>"+strings.ToLower(planLoopRunAsIs)+", please</reply>")
	if hasEvent(evs, stream.EventError) || !hasEvent(evs, stream.EventNodeDone) {
		t.Fatalf("running the plan as is: events=%v, want it to run past the judge", evs)
	}
	if _, pending := o.PendingQuestion(context.Background(), "u", "chat"); pending {
		t.Error("the choice is still pending after the user answered it")
	}
}

// TestPlanLoopQuestion_NonAppSourceHidesReason: GitHub and extension runs get fixed text naming the
// options - never the judge's reason, which can quote recalled memory.
func TestPlanLoopQuestion_NonAppSourceHidesReason(t *testing.T) {
	q := planLoopQuestion("github", "SECRET MEMORY")
	if strings.Contains(q, "SECRET") || !strings.Contains(q, planLoopRunAsIs) || !strings.Contains(q, planLoopRephrase) {
		t.Errorf("question = %q, want fixed text naming both options", q)
	}
}

func hasToolCall(evs []stream.SSEEvent, name string) bool {
	for _, ev := range evs {
		if d, ok := ev.Data.(stream.AgentToolCallData); ok && d.Name == name {
			return true
		}
	}
	return false
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
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	planner := dag.NewPlanner([]dag.AgentInfo{{Name: "web-researcher", Description: "researches the web"}}, nil, judge)
	return New(sessions, stub, func(context.Context) string { return "You are the orchestrator." }, planner, ex, nil, nil, nil)
}

// A model that gives up against an always-rejecting judge and narrates the rejection must post
// the fixed notice; the judge's reason must not appear in the answer.
func TestOrchestrator_PlanExhausted_PostsFixedNoticeNotJudgeReason(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const judgeReason = "The plan lacks a terminal node that delivers the requested artifact."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(),        // rejected
		planCallWidened(), // a different plan, rejected again
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

// One rejection then a direct answer is a pivot, not exhaustion: the answer is delivered verbatim.
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

// PlanCache is per Run(): turn one's rejections must not mark turn two exhausted.
func TestOrchestrator_RejectionDoesNotLeakAcrossTurns(t *testing.T) {
	const turnTwoAnswer = "Turn two: a plain answer, no plan involved."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(),        // turn 1: rejected
		planCallWidened(), // turn 1: a different plan rejected again -> exhausted
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

// A plan rejected once then accepted delivers normally.
func TestOrchestrator_PlanRejectedThenAccepted_NotTreatedAsExhausted(t *testing.T) {
	calls := 0
	judge := vetting.PlanJudge(func(context.Context, string, string, string) (bool, string, error) {
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
