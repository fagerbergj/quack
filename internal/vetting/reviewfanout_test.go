package vetting

import (
	"context"
	"strings"
	"testing"
	"time"
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
	resolveAbortedReviewer(context.Background(), nil, cfg, "r2")

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
