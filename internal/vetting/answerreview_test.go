package vetting

import (
	"strings"
	"testing"
)

const reviewAnswer = `The change looks mostly solid but two problems block approval.

VERDICT: request_changes
FINDINGS:
- internal/server/router.go:42: the new route is registered after the SPA fallback, so it can never match
- frontend/src/state/chatStore.ts:118: optimistic write is not rolled back on a 409
`

func TestParseAnswerReview(t *testing.T) {
	event, comments, ok := parseAnswerReview(reviewAnswer)
	if !ok || event != "request_changes" {
		t.Fatalf("verdict: ok=%v event=%q", ok, event)
	}
	if len(comments) != 2 {
		t.Fatalf("findings: got %d, want 2: %+v", len(comments), comments)
	}
	if comments[0].Path != "internal/server/router.go" || comments[0].Line != 42 || !strings.Contains(comments[0].Body, "SPA fallback") {
		t.Fatalf("first finding wrong: %+v", comments[0])
	}
	if _, _, ok := parseAnswerReview("just prose, no structure"); ok {
		t.Fatal("prose must not parse as a verdict")
	}
}

func TestAugmentFromAnswer_StagesReview(t *testing.T) {
	cfg := Config{
		ExternalWorker: true,
		ReadOnly:       true,                                                                                // a code-reviewer is read-only
		Setup:          &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"}, // a reviewer is setup-provisioned
		Task:           "Review PR #7 and post your findings as inline review comments",
	}
	act := workerActivity{}
	augmentFromAnswer(&act, cfg, reviewAnswer)
	st, ok := act.stagedDelivery["review"]
	if !ok {
		t.Fatal("review not staged")
	}
	if st.Event != "request_changes" || len(st.Comments) != 2 || st.Body != reviewAnswer {
		t.Fatalf("staged review wrong: event=%q comments=%d", st.Event, len(st.Comments))
	}

	// A verdict-less answer still stages a comment-review (never a deadlock).
	act = workerActivity{}
	augmentFromAnswer(&act, cfg, "findings in prose only")
	if st := act.stagedDelivery["review"]; st.Event != "comment" {
		t.Fatalf("verdict-less answer should stage a comment review, got %+v", st)
	}
}

func TestAugmentFromAnswer_Guards(t *testing.T) {
	reviewTask := "Review PR #7 and post your findings as inline review comments"

	// Native workers stage via the tool — the probe must not fire.
	act := workerActivity{}
	augmentFromAnswer(&act, Config{ExternalWorker: false, Task: reviewTask}, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe fired for a native worker")
	}

	// Non-review tasks (implement flows) must not stage a review.
	act = workerActivity{}
	augmentFromAnswer(&act, Config{ExternalWorker: true, Task: "Add a widget, commit, and open a pull request"}, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe fired for a non-review task")
	}

	// #471: a NON-read-only implementer whose task merely TALKS about reviews
	// (demandsPostedReview is a pure text match) must NOT stage a review — that
	// review would ride alongside its PR and 404 against the trigger issue number.
	act = workerActivity{}
	augmentFromAnswer(&act, Config{ExternalWorker: true, ReadOnly: false,
		Task: "Implement the HITL review flow: change how quack posts a review and open a pull request"}, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe staged a review for a non-read-only implementer whose task mentions reviews (#471)")
	}

	// An already-staged review always wins (reviewer path).
	setup := &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"}
	act = workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "existing"}}}
	augmentFromAnswer(&act, Config{ExternalWorker: true, ReadOnly: true, Setup: setup, Task: reviewTask}, reviewAnswer)
	if act.stagedDelivery["review"].Body != "existing" {
		t.Fatal("probe replaced a staged review")
	}

	// #482: a read-only node whose task mentions reviews but has NO Setup (a
	// code-explorer investigating the review path on an ISSUE, not a PR) must NOT
	// stage a review — delivery would then fail with "'' is not a github.com
	// clone URL". Only a setup-provisioned reviewer stages.
	act = workerActivity{}
	augmentFromAnswer(&act, Config{ExternalWorker: true, ReadOnly: true, Task: reviewTask}, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe staged a review with no Setup (no PR to review against) (#482)")
	}
}
