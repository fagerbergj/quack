package orchestrator

import (
	"context"
	"time"

	"testing"

	"google.golang.org/adk/v2/session"
)

// TestRetryNodeResumedSkipsRunAdmission pins #1176: a boot resume rides
// RetryNodeResumed, which must run even while the one run slot is held by
// another run - the resumed node was already admitted by the process that
// died, and that reservation is gone, so re-acquiring here would starve new
// work out of the admission queue forever.
func TestRetryNodeResumedSkipsRunAdmission(t *testing.T) {
	o := &Orchestrator{sessions: session.InMemoryService()}
	o.SetMaxActiveRuns(1)

	hold, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the one slot")
	}
	defer hold()

	done := make(chan struct{})
	go func() {
		// No stashed plan exists, so this yields an error and returns fast -
		// what matters is that it never blocks waiting on the held slot.
		for range o.RetryNodeResumed(context.Background(), "u1", "c1", nil, "n1", "") {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RetryNodeResumed blocked on the run admission slot - it must skip acquireRun")
	}
}

// TestRetryNodeStillConsumesRunAdmission is TestRetryNodeResumedSkipsRunAdmission's
// counterpart: a REST-triggered retry (RetryNode, not RetryNodeResumed) is a
// fresh dispatch and must still wait for a slot like any other run.
func TestRetryNodeStillConsumesRunAdmission(t *testing.T) {
	o := &Orchestrator{sessions: session.InMemoryService()}
	o.SetMaxActiveRuns(1)

	hold, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the one slot")
	}

	done := make(chan struct{})
	go func() {
		for range o.RetryNode(context.Background(), "u1", "c1", nil, "n1", "") {
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("RetryNode returned before the slot freed - it is not going through admission")
	case <-time.After(100 * time.Millisecond):
		// correctly blocked waiting on the held slot
	}
	hold()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RetryNode never proceeded after the slot freed")
	}
}
