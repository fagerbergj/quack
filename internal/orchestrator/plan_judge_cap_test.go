package orchestrator

import (
	"context"
	"strings"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// Real plan-judge rejection reasons captured from the QA rig's non-convergent
// PR-review-fixture run (~/workspace/wt/qa/result-18a8d57.md,
// ~/workspace/wt/qa/logs/server.log): the judge reworded the same underlying
// complaint - the proposed plan modifies README.md instead of reviewing the
// change - every round, so no two consecutive reasons were byte-identical.
// This regression fixture proves the round cap, not the repeated-reason
// stop, is what has to end a loop shaped like the rig's.
var rigRejectionReasons = []string{
	`The user's request explicitly demands execution of a review or change with tools, not just a plan. The proposed plan is a single code-implementer node that modifies the README.md but lacks any node performing the requested "read the change" and "carry out the review" steps. It fails to deliver what the user asked for: an actual review/examination of changes, not merely adding a line to a file.`,
	`The user explicitly requested to "read the change, and carry out the review" - meaning they want a code review of an existing PR, not implementation of changes. The proposed plan incorrectly sets up a code-implementer node to make changes (append a line to README.md) instead of creating a reviewer node that reads and reviews the actual PR.`,
	`The user explicitly requested a review of the change, not an implementation. The proposed plan incorrectly implements a change (appending a line to README.md) instead of performing the requested review.`,
	`The user's request explicitly demands a review of changes and the execution of tools (clone, read change, carry out review), not just an implementation task. The proposed plan only contains a code-implementer node to append a line to README.md, which does not address the request to clone the repo, read the change, and carry out a review. The plan needs a code-reviewer node to perform the actual review of changes.`,
	`The user explicitly requested a live execution of a review, not an implementation plan. The proposed plan is a code-implementer node that modifies a file and creates a pull request, which does not fulfill the user's request for a "review".`,
	`The user explicitly requested a review of changes, not an implementation task. The proposed plan's node-1 is tasked with cloning the repo and modifying README.md (implementation), which does not match the request for a code review.`,
}

// rigJudge replays rigRejectionReasons in order, cycling - mirrors the rig's
// judge rejecting every retry with a freshly-worded version of the same complaint.
func rigJudge() vetting.PlanJudge {
	i := 0
	return func(context.Context, string, string, string) (bool, string, error) {
		reason := rigRejectionReasons[i%len(rigRejectionReasons)]
		i++
		return false, reason, nil
	}
}

func planCalls(n int) []*model.LLMResponse {
	out := make([]*model.LLMResponse, n)
	for i := range out {
		out[i] = planCall()
	}
	return out
}

// TestOrchestrator_PlanJudgeCap_RigRegressionText pins the QA rig's actual
// failure (result-18a8d57.md: 56 rejections/347s, no cap, nothing delivered):
// replaying real rejection text against the default round cap (5) must stop
// the loop there and deliver a failure that carries those reasons, not hang.
// capAwareModel grants one real grace round after the cap trips (#760: a
// pivot must survive), so a model that keeps calling plan costs the round
// cap PLUS that one grace round in real invocations before the synthetic
// round ends the turn - 6 here, not 5.
func TestOrchestrator_PlanJudgeCap_RigRegressionText(t *testing.T) {
	stub := &orchStub{replies: planCalls(8)} // more than cap+grace - proves it actually stops there
	o := newTestOrchWithJudge(t, stub, rigJudge())

	evs := runTurn(t, o, "clone the repo, read the change, and carry out the review")

	if got := stub.invocations(); got != 6 {
		t.Fatalf("model invocations = %d, want exactly 6 (the round cap of 5 plus one grace round) - a run with no cap consumed all 8 queued rounds and more (rig: 56)", got)
	}
	if !hasEvent(evs, stream.EventError) {
		t.Errorf("a capped run must surface an error event; events=%v", evs)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	const wantPrefix = "The planner could not produce an acceptable plan: "
	if !strings.HasPrefix(answer, wantPrefix) {
		t.Fatalf("answer = %q, want it to start with %q", answer, wantPrefix)
	}
	if answer == planExhaustedNotice {
		t.Fatal("a capped run must deliver the judge's reasons, not the generic reason-suppressing exhaustion notice")
	}
	for _, reason := range rigRejectionReasons[:6] {
		if !strings.Contains(answer, reason) {
			t.Errorf("answer missing an actual rejection reason from the fixture: %q", reason)
		}
	}
}

// TestOrchestrator_PlanJudgeCap_Configurable proves SetPlanJudgeCap's round
// cap is honored (not just the built-in default of 5): with distinct
// reasons every round (so the repeat-reason stop never trips first), a cap
// of 2 must stop the loop after the 2 capped rounds plus one grace round.
func TestOrchestrator_PlanJudgeCap_Configurable(t *testing.T) {
	stub := &orchStub{replies: planCalls(6)}
	o := newTestOrchWithJudge(t, stub, rigJudge())
	o.SetPlanJudgeCap(2, 100)

	runTurn(t, o, "clone the repo, read the change, and carry out the review")

	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want exactly the configured round cap (2) plus one grace round", got)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	for _, reason := range rigRejectionReasons[:3] {
		if !strings.Contains(answer, reason) {
			t.Errorf("answer = %q, missing round's reason %q", answer, reason)
		}
	}
}

// TestOrchestrator_PlanJudgeRepeatedReason_StopsBeforeRoundCap: repeating
// the identical rejection reason is not converging - it must end the turn
// at the repeat cap (3) plus one grace round, well under the round cap of 5.
func TestOrchestrator_PlanJudgeRepeatedReason_StopsBeforeRoundCap(t *testing.T) {
	const reason = "delivery not declared: the plan has no terminal node that delivers the requested artifact"
	stub := &orchStub{replies: planCalls(8)}
	o := newTestOrchWithJudge(t, stub, rejectAlwaysJudge(reason))

	runTurn(t, o, "make a small follow-up change")

	if got := stub.invocations(); got != 4 {
		t.Fatalf("model invocations = %d, want exactly 4 (the repeat cap of 3 plus one grace round) - repeating the same reason must stop the loop early, before the round cap (5)", got)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	const wantPrefix = "The planner could not produce an acceptable plan: "
	if !strings.HasPrefix(answer, wantPrefix) {
		t.Fatalf("answer = %q, want it to start with %q", answer, wantPrefix)
	}
	if n := strings.Count(answer, reason); n != 1 {
		t.Errorf("answer repeats the identical reason %d times, want it deduped to 1: %q", n, answer)
	}
}

// TestOrchestrator_PlanJudgeCap_GracePivotDeliveredVerbatim: once the round
// cap trips, the model gets exactly one more real round to react. A genuine
// pivot to a direct answer (#760) in that round must reach the user
// verbatim - it must NOT be replaced by the cap notice just because the
// round count is already high, and the round cap's own exhaustion check
// must not clobber it either.
func TestOrchestrator_PlanJudgeCap_GracePivotDeliveredVerbatim(t *testing.T) {
	const pivotAnswer = "This request doesn't need a plan - here's the direct answer."
	stub := &orchStub{replies: []*model.LLMResponse{
		planCall(), planCall(), // rounds 1-2: rejected, trips the round cap of 2
		stubText(pivotAnswer), // round 3 (grace): a genuine pivot, not another plan call
	}}
	o := newTestOrchWithJudge(t, stub, rigJudge())
	o.SetPlanJudgeCap(2, 100)

	evs := runTurn(t, o, "clone the repo, read the change, and carry out the review")

	if hasEvent(evs, stream.EventError) {
		t.Errorf("a grace-round pivot must not surface an error; events=%v", evs)
	}
	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want 3 (2 capped rounds + the one grace round)", got)
	}
	if answer := o.LatestAnswer(context.Background(), "u", "chat"); answer != pivotAnswer {
		t.Errorf("answer = %q, want the model's own grace-round pivot %q delivered verbatim", answer, pivotAnswer)
	}
}

// newTracedTestOrchWithJudge is newTracedTestOrch (ledger_coords_test.go)
// with a plan judge wired in - the orchestrator's own top-level model is
// traced like production's inference.NewModel/serve.go does, so a synthetic
// completion routed through the same wrapper (capAwareModel) is provably
// observable the same way a real one is.
func newTracedTestOrchWithJudge(t *testing.T, stub *orchStub, judge vetting.PlanJudge) *Orchestrator {
	t.Helper()
	tracedStub := inference.TracedModelForTesting(stub, "orch-test-model")
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
	return New(sessions, tracedStub, "You are the orchestrator.", planner, ex, nil, nil, nil)
}

// TestOrchestrator_PlanJudgeCap_SyntheticRoundRecordsLedgerEntry: the
// synthetic completion capAwareModel returns once the grace round is spent
// bypasses the real model - it must still leave a normal llm.call ledger
// trail (via inference.TracedModelForTesting), not vanish from observability
// just because no real backend was called.
func TestOrchestrator_PlanJudgeCap_SyntheticRoundRecordsLedgerEntry(t *testing.T) {
	capExp := &ledgerCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	const reason = "delivery not declared: the plan has no terminal node that delivers the requested artifact"
	stub := &orchStub{replies: planCalls(8)}
	o := newTracedTestOrchWithJudge(t, stub, rejectAlwaysJudge(reason))
	o.SetPlanJudgeCap(2, 2) // repeat cap 2: capped after round 2, grace at round 3, synthetic at round 4

	runTurn(t, o, "make a small follow-up change")

	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want 3 (2 capped rounds + one grace round)", got)
	}

	var synthetic map[string]string
	chatCalls := 0
	for _, r := range capExp.records {
		attrs := ledgerAttrsOf(r)
		if attrs["gen_ai.operation.name"] != "chat" {
			continue
		}
		chatCalls++
		if attrs["gen_ai.request.model"] == planJudgeCapModelName {
			synthetic = attrs
		}
	}
	if synthetic == nil {
		t.Fatal("no llm.call ledger event recorded for the synthetic round - it must be observable like a real call")
	}
	if got := synthetic["gen_ai.conversation.id"]; got != "chat" {
		t.Errorf("synthetic round's gen_ai.conversation.id = %q, want the chat id, same as a real call", got)
	}
	// 3 real rounds (2 capped + grace) plus the one synthetic completion.
	if chatCalls != 4 {
		t.Errorf("recorded %d chat ledger events, want 4 (3 real + 1 synthetic)", chatCalls)
	}
}
