package orchestrator

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

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
func TestOrchestrator_PlanJudgeCap_RigRegressionText(t *testing.T) {
	stub := &orchStub{replies: planCalls(8)} // more than the round cap - proves it actually stops there
	o := newTestOrchWithJudge(t, stub, rigJudge())

	evs := runTurn(t, o, "clone the repo, read the change, and carry out the review")

	if got := stub.invocations(); got != 5 {
		t.Fatalf("model invocations = %d, want exactly the round cap (5) rounds - a run with no cap consumed all 8 queued rounds and more (rig: 56)", got)
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
	for _, reason := range rigRejectionReasons[:5] {
		if !strings.Contains(answer, reason) {
			t.Errorf("answer missing an actual rejection reason from the fixture: %q", reason)
		}
	}
}

// TestOrchestrator_PlanJudgeCap_Configurable proves SetPlanJudgeCap's round
// cap is honored (not just the built-in default of 5): with distinct
// reasons every round (so the repeat-reason stop never trips first), a cap
// of 2 must stop the loop at 2 rounds.
func TestOrchestrator_PlanJudgeCap_Configurable(t *testing.T) {
	stub := &orchStub{replies: planCalls(6)}
	o := newTestOrchWithJudge(t, stub, rigJudge())
	o.SetPlanJudgeCap(2, 100)

	runTurn(t, o, "clone the repo, read the change, and carry out the review")

	if got := stub.invocations(); got != 2 {
		t.Fatalf("model invocations = %d, want exactly the configured round cap (2)", got)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, rigRejectionReasons[0]) || !strings.Contains(answer, rigRejectionReasons[1]) {
		t.Errorf("answer = %q, want both of the two rounds' reasons", answer)
	}
}

// TestOrchestrator_PlanJudgeRepeatedReason_StopsBeforeRoundCap: repeating
// the identical rejection reason is not converging - it must end the turn
// at the repeat cap (3, well under the round cap of 5), not wait for the
// round cap to also be reached.
func TestOrchestrator_PlanJudgeRepeatedReason_StopsBeforeRoundCap(t *testing.T) {
	const reason = "delivery not declared: the plan has no terminal node that delivers the requested artifact"
	stub := &orchStub{replies: planCalls(8)}
	o := newTestOrchWithJudge(t, stub, rejectAlwaysJudge(reason))

	runTurn(t, o, "make a small follow-up change")

	if got := stub.invocations(); got != 3 {
		t.Fatalf("model invocations = %d, want exactly the repeat cap (3) - repeating the same reason must stop the loop early, before the round cap (5)", got)
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
