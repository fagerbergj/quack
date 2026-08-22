package vetting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/workflow"
)

// Multi-reviewer plan (#867): three reviewer nodes fan out; nothing may be
// delivered until every one of them has gone terminal, and the eventual
// delivery must be exactly one, worst-of verdict, with findings attributed.
func TestReviewFanout_DeliversOnceWorstOfWhenAllTerminal(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		select {
		case done <- dc:
		default:
			t.Error("Deliver called more than once")
		}
		return nil, nil
	}
	fanout := GetReviewFanout(t.Name(), 3)
	cfg := Config{Deliver: deliver, ReviewFanout: fanout, IsReviewer: true}

	// First reviewer stages approve and finishes - nothing delivered yet.
	commitDelivery(context.Background(), nil, cfg, "r1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "r1 looks fine"}},
	}, GateResult{Passed: true})
	select {
	case dc := <-done:
		t.Fatalf("delivered after only 1 of 3 reviewers finished: %+v", dc)
	case <-time.After(50 * time.Millisecond):
	}

	// Second reviewer stages request_changes and finishes - still nothing delivered.
	commitDelivery(context.Background(), nil, cfg, "r2", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "request_changes", Body: "r2 found a bug",
			Comments: []ReviewComment{{Path: "a.go", Line: 3, Body: "nil deref"}}}},
	}, GateResult{Passed: true})
	select {
	case dc := <-done:
		t.Fatalf("delivered after only 2 of 3 reviewers finished: %+v", dc)
	case <-time.After(50 * time.Millisecond):
	}

	// Third and last reviewer finishes - exactly one delivery now, worst-of verdict.
	commitDelivery(context.Background(), nil, cfg, "r3", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "r3 looks fine"}},
	}, GateResult{Passed: true})
	select {
	case dc := <-done:
		if len(dc.Items) != 1 || dc.Items[0].Kind != "review" {
			t.Fatalf("Items = %+v, want exactly one merged review item", dc.Items)
		}
		item := dc.Items[0]
		if item.Event != "request_changes" {
			t.Fatalf("Event = %q, want request_changes (worst-of beats approve)", item.Event)
		}
		if !strings.Contains(item.Body, "r1 looks fine") || !strings.Contains(item.Body, "r2 found a bug") || !strings.Contains(item.Body, "r3 looks fine") {
			t.Fatalf("Body = %q, want findings from all three nodes", item.Body)
		}
		if len(item.Comments) != 1 || !strings.Contains(item.Comments[0].Body, "r2") {
			t.Fatalf("Comments = %+v, want r2's finding attributed to r2", item.Comments)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery after every reviewer node finished")
	}
}

// All-approve multi-reviewer plan: one delivery, approve.
func TestReviewFanout_AllApprovesDeliverApprove(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	fanout := GetReviewFanout(t.Name(), 2)
	cfg := Config{Deliver: deliver, ReviewFanout: fanout, IsReviewer: true}

	commitDelivery(context.Background(), nil, cfg, "r1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "fine"}},
	}, GateResult{Passed: true})
	commitDelivery(context.Background(), nil, cfg, "r2", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "also fine"}},
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		if len(dc.Items) != 1 || dc.Items[0].Event != "approve" {
			t.Fatalf("Items = %+v, want a single approve", dc.Items)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery after both reviewers approved")
	}
}

// Single-reviewer plan: cfg.ReviewFanout is nil, so commitDelivery must
// behave exactly as it did before #867 - this pins the regression risk.
func TestReviewFanout_SingleReviewerPlanUnchanged(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	cfg := Config{Deliver: deliver, IsReviewer: true} // ReviewFanout nil: single-reviewer plan
	commitDelivery(context.Background(), nil, cfg, "solo", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "ship it"}},
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		if len(dc.Items) != 1 || dc.Items[0].Event != "approve" || dc.Items[0].Body != "ship it" {
			t.Fatalf("Items = %+v, want the node's own review delivered directly and unmerged", dc.Items)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a single-reviewer node must deliver its own review immediately, exactly like before #867")
	}
}

// Staging seam defense-in-depth: an approve must not stage while a sibling
// reviewer node is still running, but a request_changes may.
func TestReviewStage_RefusesEarlyApproveAllowsEarlyRequestChanges(t *testing.T) {
	fanout := GetReviewFanout(t.Name(), 2) // 2 reviewers, neither terminal yet
	stage := NewReviewStage(fanout)

	if err := stage.SetVerdict("approve", "looks good"); err == nil {
		t.Fatal("approve must be refused while a sibling reviewer is still pending")
	}
	if _, ok := stage.Snapshot(); ok {
		t.Fatal("a refused verdict must not be staged")
	}
	if err := stage.SetVerdict("request_changes", "found a bug"); err != nil {
		t.Fatalf("request_changes must be allowed to stage early: %v", err)
	}
	sd, ok := stage.Snapshot()
	if !ok || sd.Event != "request_changes" {
		t.Fatalf("Snapshot = %+v ok=%v, want the staged request_changes", sd, ok)
	}

	// Once every sibling is terminal, approve is no longer refused.
	fanout.Finish("other-reviewer", StagedDelivery{}, false, true)
	soloStage := NewReviewStage(fanout)
	if err := soloStage.SetVerdict("approve", "now fine"); err != nil {
		t.Fatalf("approve should be allowed once every sibling is terminal: %v", err)
	}
}

// A reviewer node that errors or is cancelled must not block the run's
// delivery forever (#867 sibling-failure policy: deliver what you have,
// with a note - never silently swallow the surviving reviews, never hang).
func TestReviewFanout_FailedSiblingDoesNotBlockDelivery(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	fanout := GetReviewFanout(t.Name(), 2)
	cfg := Config{Deliver: deliver, ReviewFanout: fanout, IsReviewer: true}

	// r1 finishes normally with a request_changes.
	commitDelivery(context.Background(), nil, cfg, "r1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "request_changes", Body: "found a bug"}},
	}, GateResult{Passed: true})

	// r2 errored/was cancelled - never reaches commitDelivery. The abort path
	// (resolveAbortedReviewer, wired via RunGatedRefine's defer) registers it.
	resolveAbortedReviewer(context.Background(), nil, cfg, "r2", workerActivity{})

	select {
	case dc := <-done:
		if len(dc.Items) != 1 {
			t.Fatalf("Items = %+v, want exactly one delivery despite the failed sibling", dc.Items)
		}
		item := dc.Items[0]
		if item.Event != "request_changes" {
			t.Fatalf("Event = %q, want the surviving reviewer's verdict", item.Event)
		}
		if !strings.Contains(item.Body, "r2") {
			t.Fatalf("Body = %q, want the failed sibling named, not silently dropped", item.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a failed sibling must not block delivery forever")
	}
}

// #942: a single-reviewer plan (no ReviewFanout) whose round dies (killed,
// timed out, errored) after quackmcp_stage_review already staged the full
// verdict must deliver that staged review, flagged as abnormal - not discard
// it and not substitute anything else.
func TestResolveAbortedReviewer_SoloReviewerDeliversStagedReviewInsteadOfDiscarding(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	cfg := Config{Deliver: deliver, IsReviewer: true}
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Body: "verified end to end, holds up"},
	}}

	resolveAbortedReviewer(context.Background(), nil, cfg, "review-935", act)

	select {
	case dc := <-done:
		if len(dc.Items) != 1 || dc.Items[0].Event != "approve" {
			t.Fatalf("Items = %+v, want the staged approve delivered", dc.Items)
		}
		if !strings.Contains(dc.Items[0].Body, "verified end to end") {
			t.Fatalf("Body = %q, want the staged verdict preserved", dc.Items[0].Body)
		}
		if !strings.Contains(dc.Items[0].Body, "abnormally") {
			t.Fatalf("Body = %q, want a note that the round ended abnormally", dc.Items[0].Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a staged review must be delivered when the round that produced it dies")
	}
}

// A solo reviewer's round that dies with nothing staged must deliver nothing
// (no ReviewFanout to fall back on, and nothing true to say).
func TestResolveAbortedReviewer_SoloReviewerNothingStagedDeliversNothing(t *testing.T) {
	deliverCalled := false
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		deliverCalled = true
		return nil, nil
	}
	cfg := Config{Deliver: deliver, IsReviewer: true}

	resolveAbortedReviewer(context.Background(), nil, cfg, "review-936", workerActivity{})

	if deliverCalled {
		t.Fatal("nothing was staged; Deliver must not be called")
	}
}

// isReviewerPauseSentinel: both pause sentinels exclude a node from the
// abort path; a real dead end (nil w/ no delivery, or a plain error) does
// not.
func TestIsReviewerPauseSentinel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrNodePaused", ErrNodePaused, true},
		{"workflow.ErrNodeInterrupted", workflow.ErrNodeInterrupted, true},
		{"wrapped ErrNodeInterrupted", fmt.Errorf("park: %w", workflow.ErrNodeInterrupted), true},
		{"plain error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := isReviewerPauseSentinel(c.err); got != c.want {
			t.Errorf("%s: isReviewerPauseSentinel = %v, want %v", c.name, got, c.want)
		}
	}
}

// A reviewer node parked on a human question (workflow.ErrNodeInterrupted,
// ADK's HITL park sentinel, returned by pauseIfWorkerRaisedHITL) must NOT be
// registered as a failed terminal - it isn't done yet. Nothing may deliver
// while it's parked; once it resumes and stages its real verdict, the fan-in
// completes and that verdict is included, not discarded (#948 review finding).
func TestReviewFanout_ParkedReviewerNotCountedFailedThenResumesIntoDelivery(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	fanout := GetReviewFanout(t.Name(), 2)
	cfg := Config{Deliver: deliver, ReviewFanout: fanout, IsReviewer: true}

	// r1 finishes normally with request_changes.
	commitDelivery(context.Background(), nil, cfg, "r1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "request_changes", Body: "found a bug"}},
	}, GateResult{Passed: true})

	// r2 parks on a human question - RunGatedRefine's defer sees
	// workflow.ErrNodeInterrupted and, per isReviewerPauseSentinel, must NOT
	// call resolveAbortedReviewer.
	if !isReviewerPauseSentinel(workflow.ErrNodeInterrupted) {
		t.Fatal("workflow.ErrNodeInterrupted must be treated as a pause, not a failure")
	}
	// (deliberately NOT calling resolveAbortedReviewer for r2 here - that's the point)

	select {
	case dc := <-done:
		t.Fatalf("delivered while r2 is still parked: %+v", dc)
	case <-time.After(50 * time.Millisecond):
	}

	// r2 resumes and finishes with its own verdict.
	commitDelivery(context.Background(), nil, cfg, "r2", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "resumed, looks fine"}},
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		if len(dc.Items) != 1 {
			t.Fatalf("Items = %+v, want exactly one merged delivery", dc.Items)
		}
		item := dc.Items[0]
		if item.Event != "request_changes" {
			t.Fatalf("Event = %q, want worst-of (request_changes)", item.Event)
		}
		if !strings.Contains(item.Body, "resumed, looks fine") {
			t.Fatalf("Body = %q, want the parked node's resumed verdict included, not discarded", item.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery after the parked reviewer resumed and staged its verdict")
	}
}

// Review plan with a downstream synthesizer (the PR #965 incident, 03:46Z):
// two reviewer nodes finish, but the plan's synthesizer node owns the final
// consolidated review - nothing may go to GitHub until it finishes, and the
// one delivery must carry the synthesizer's body, worst-of verdict, and the
// reviewers' attributed inline comments.
func TestReviewFanout_SynthesizerOwnsDelivery(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		select {
		case done <- dc:
		default:
			t.Error("Deliver called more than once")
		}
		return nil, nil
	}
	fanout := GetReviewFanout(t.Name(), 2)
	fanout.ExpectSynthesis()
	cfg := Config{Deliver: deliver, ReviewFanout: fanout, IsReviewer: true}

	commitDelivery(context.Background(), nil, cfg, "review-backend", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "request_changes", Body: "backend bug",
			Comments: []ReviewComment{{Path: "a.go", Line: 3, Body: "nil deref"}}}},
	}, GateResult{Passed: true})
	commitDelivery(context.Background(), nil, cfg, "review-frontend", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "frontend fine"}},
	}, GateResult{Passed: true})
	select {
	case dc := <-done:
		t.Fatalf("delivered before the synthesizer node finished: %+v", dc)
	case <-time.After(50 * time.Millisecond):
	}

	// Synthesizer node (not a reviewer, stages nothing) finishes with the
	// consolidated review as its answer - now exactly one delivery.
	synthCfg := Config{Deliver: deliver, ReviewFanout: fanout}
	commitDelivery(context.Background(), nil, synthCfg, "synthesize", workerActivity{
		answer: "Consolidated: fix the nil deref, frontend is fine.",
	}, GateResult{Passed: true})

	select {
	case dc := <-done:
		if len(dc.Items) != 1 || dc.Items[0].Kind != "review" {
			t.Fatalf("Items = %+v, want exactly one review item", dc.Items)
		}
		item := dc.Items[0]
		if item.Body != "Consolidated: fix the nil deref, frontend is fine." {
			t.Fatalf("Body = %q, want the synthesizer's answer, not the raw per-node concatenation", item.Body)
		}
		if item.Event != "request_changes" {
			t.Fatalf("Event = %q, want request_changes (worst-of)", item.Event)
		}
		if len(item.Comments) != 1 || !strings.Contains(item.Comments[0].Body, "review-backend") {
			t.Fatalf("Comments = %+v, want the backend finding attributed", item.Comments)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no delivery after the synthesizer finished")
	}
}

// A dead synthesizer must not strand the reviews: the fan-in falls back to
// the merged per-node concatenation instead of delivering nothing.
func TestReviewFanout_SynthesizerAbortFallsBackToConcat(t *testing.T) {
	done := make(chan DeliveryContext, 1)
	deliver := func(_ context.Context, dc DeliveryContext) ([]DeliveryItemOutcome, error) {
		done <- dc
		return nil, nil
	}
	fanout := GetReviewFanout(t.Name(), 2)
	fanout.ExpectSynthesis()
	cfg := Config{Deliver: deliver, ReviewFanout: fanout, IsReviewer: true}

	commitDelivery(context.Background(), nil, cfg, "r1", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "fine"}},
	}, GateResult{Passed: true})
	commitDelivery(context.Background(), nil, cfg, "r2", workerActivity{
		stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "also fine"}},
	}, GateResult{Passed: true})

	// Synthesizer errored before producing an answer.
	resolveAbortedReviewer(context.Background(), nil, Config{Deliver: deliver, ReviewFanout: fanout}, "synthesize", workerActivity{})

	select {
	case dc := <-done:
		if len(dc.Items) != 1 {
			t.Fatalf("Items = %+v, want the merged reviews delivered anyway", dc.Items)
		}
		if !strings.Contains(dc.Items[0].Body, "fine") {
			t.Fatalf("Body = %q, want the reviewers' own findings as fallback", dc.Items[0].Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("aborted synthesizer stranded the staged reviews")
	}
}
