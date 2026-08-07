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
	c, ok := danglingDeliverablePathCriterion(answer, act, "node1")
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
	if _, ok := danglingDeliverablePathCriterion(answer, act, "node1"); ok {
		t.Fatal("a committed file's path must not be flagged as dangling")
	}
}

// TestDanglingDeliverablePathCriterion_NoWrittenPathMentioned: the answer
// happens to mention an unrelated filename - only a path THIS run actually
// wrote (act.written) can trigger the criterion.
func TestDanglingDeliverablePathCriterion_NoWrittenPathMentioned(t *testing.T) {
	act := workerActivity{written: []string{"node1/scratch.tmp"}}
	answer := "See `main.go` for the entrypoint; the router is wired in `router.go`."
	if _, ok := danglingDeliverablePathCriterion(answer, act, "node1"); ok {
		t.Fatal("flagged a path the run never mentioned")
	}
}

func TestDanglingDeliverablePathCriterion_NothingWritten(t *testing.T) {
	if _, ok := danglingDeliverablePathCriterion("some findings", workerActivity{}, "node1"); ok {
		t.Fatal("fired with no written files at all")
	}
}

// TestDanglingDeliverablePathCriterion_DiscussingAnEditedFile pins the
// home-server#3 false positive (github-fagerbergj-home-server-3, node
// "explore-llm"): a code-explorer run strayed off-task and edited
// deepwiki/docker-compose.yml, but its actual answer only ever CITES that
// path (and the unrelated llm/docker-compose.yml it was asked to read) in a
// findings table - it never claims the file IS the deliverable. Basename
// substring matching alone flags this because "docker-compose.yml" is a
// common name the answer legitimately discusses in prose; this must not fire.
//
// This reproduces against current main - see PR description for the recorded
// run (chat github-fagerbergj-home-server-3).
func TestDanglingDeliverablePathCriterion_DiscussingAnEditedFile(t *testing.T) {
	act := workerActivity{written: []string{"explore-llm/deepwiki/docker-compose.yml"}}
	answer := "### 1. Full docker-compose.yml — `llm/docker-compose.yml`\n\n" +
		"three services in one compose file at `llm/docker-compose.yml:1-89`.\n\n" +
		"| Service | Compose file | Connection string |\n" +
		"|---------|-------------|-------------------|\n" +
		"| **deepwiki** | `deepwiki/docker-compose.yml:L11` | `LITELLM_BASE_URL=http://llm-swap:11436` |\n"
	if c, ok := danglingDeliverablePathCriterion(answer, act, "explore-llm"); ok {
		t.Fatalf("false positive: flagged a legitimately-cited file as a dangling deliverable pointer; reason=%q", c.Reason)
	}
}

// TestDanglingDeliverablePathCriterion_BasenameFallbackStillNeedsPointerPhrase:
// a bare-basename occurrence (no directory prefix) still must not fire
// without pointer language nearby.
func TestDanglingDeliverablePathCriterion_BasenameFallbackStillNeedsPointerPhrase(t *testing.T) {
	act := workerActivity{written: []string{"node1/sub/docker-compose.yml"}}
	answer := "Reading `docker-compose.yml` shows the deepwiki service routes through llm-swap."
	if c, ok := danglingDeliverablePathCriterion(answer, act, "node1"); ok {
		t.Fatalf("false positive: flagged %q", c.Reason)
	}
}

// TestDanglingDeliverablePathCriterion_BasenameFallbackWithPointerPhrase: the
// full relative path never appears (only the bare filename), but the
// pointer phrasing is genuine - this is still the true positive the
// criterion exists for.
func TestDanglingDeliverablePathCriterion_BasenameFallbackWithPointerPhrase(t *testing.T) {
	act := workerActivity{written: []string{"node1/sub/report.md"}}
	answer := "The full write-up is saved to `report.md`."
	c, ok := danglingDeliverablePathCriterion(answer, act, "node1")
	if !ok {
		t.Fatal("want ok=true - bare-basename pointer language is still a dangling pointer")
	}
	if !strings.Contains(c.Reason, "report.md") {
		t.Errorf("Reason should name the dangling path; got %q", c.Reason)
	}
}
