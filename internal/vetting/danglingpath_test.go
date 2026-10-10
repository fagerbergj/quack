package vetting

import (
	"strings"
	"testing"
)

// A plan-only run that wrote a file into its discarded working dir and pointed at it as the deliverable,
// never committing, must fail.
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

// A committed write ships on the delivered branch, so referencing it is not dangling.
func TestDanglingDeliverablePathCriterion_CommittedIsFine(t *testing.T) {
	act := workerActivity{written: []string{"node1/PLAN.md"}, committed: true}
	answer := "Committed the plan to `PLAN.md` on the work branch."
	if _, ok := danglingDeliverablePathCriterion(answer, act, "node1"); ok {
		t.Fatal("a committed file's path must not be flagged as dangling")
	}
}

// Only a path this run actually wrote (act.written) can trigger the criterion.
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

// An answer that only cites an edited file in a findings table never claims it is the deliverable; a common
// basename like docker-compose.yml must not fire on substring match alone.
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

// A bare-basename occurrence still must not fire without pointer language nearby.
func TestDanglingDeliverablePathCriterion_BasenameFallbackStillNeedsPointerPhrase(t *testing.T) {
	act := workerActivity{written: []string{"node1/sub/docker-compose.yml"}}
	answer := "Reading `docker-compose.yml` shows the deepwiki service routes through llm-swap."
	if c, ok := danglingDeliverablePathCriterion(answer, act, "node1"); ok {
		t.Fatalf("false positive: flagged %q", c.Reason)
	}
}

// Only the bare filename appears, but the pointer phrasing is genuine: still a true positive.
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
