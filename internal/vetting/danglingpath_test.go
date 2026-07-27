package vetting

import (
	"strings"
	"testing"
)

// TestDanglingDeliverablePathCriterion_CatchesUncommittedPointer pins #569:
// a plan-only run wrote PLAN_58_HOME_FRAGMENT_COMPOSE.md into its (discarded)
// working directory and then posted a comment pointing at it as the
// deliverable, never having committed anything.
func TestDanglingDeliverablePathCriterion_CatchesUncommittedPointer(t *testing.T) {
	act := workerActivity{written: []string{"node1/PLAN_58_HOME_FRAGMENT_COMPOSE.md"}}
	answer := "The implementation plan is complete at `PLAN_58_HOME_FRAGMENT_COMPOSE.md`. Here's what it covers: ..."
	c, ok := danglingDeliverablePathCriterion(answer, act)
	if !ok {
		t.Fatal("want ok=true - the answer points at a written-but-uncommitted file")
	}
	if c.Score != 0 {
		t.Fatalf("Score = %v, want 0", c.Score)
	}
	if !strings.Contains(c.Reason, "PLAN_58_HOME_FRAGMENT_COMPOSE.md") {
		t.Errorf("Reason should name the dangling path; got %q", c.Reason)
	}
}

// TestDanglingDeliverablePathCriterion_CommittedIsFine: a committed write
// ships on the branch/PR the delivery step pushes - referencing it is not
// dangling.
func TestDanglingDeliverablePathCriterion_CommittedIsFine(t *testing.T) {
	act := workerActivity{written: []string{"node1/PLAN.md"}, committed: true}
	answer := "Committed the plan to `PLAN.md` on the work branch."
	if _, ok := danglingDeliverablePathCriterion(answer, act); ok {
		t.Fatal("a committed file's path must not be flagged as dangling")
	}
}

// TestDanglingDeliverablePathCriterion_NoWrittenPathMentioned: the answer
// happens to mention an unrelated filename - only a path THIS run actually
// wrote (act.written) can trigger the criterion.
func TestDanglingDeliverablePathCriterion_NoWrittenPathMentioned(t *testing.T) {
	act := workerActivity{written: []string{"node1/scratch.tmp"}}
	answer := "See `main.go` for the entrypoint; the router is wired in `router.go`."
	if _, ok := danglingDeliverablePathCriterion(answer, act); ok {
		t.Fatal("flagged a path the run never mentioned")
	}
}

func TestDanglingDeliverablePathCriterion_NothingWritten(t *testing.T) {
	if _, ok := danglingDeliverablePathCriterion("some findings", workerActivity{}); ok {
		t.Fatal("fired with no written files at all")
	}
}
