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

// TestStripVerdictTail pins #482: the own-PR comment path must not leak the
// machine-parseable VERDICT/FINDINGS tail or a fallback-format preamble.
func TestStripVerdictTail(t *testing.T) {
	got := StripVerdictTail(reviewAnswer)
	if strings.Contains(got, "VERDICT:") || strings.Contains(got, "FINDINGS:") {
		t.Fatalf("tail leaked: %q", got)
	}
	if !strings.Contains(got, "two problems block approval") {
		t.Fatalf("human-facing summary dropped: %q", got)
	}

	withPreamble := "Since staging tools aren't available in this environment, here is the full structured review as the fallback output format:\n\n" + reviewAnswer
	got = StripVerdictTail(withPreamble)
	if strings.Contains(got, "fallback output format") {
		t.Fatalf("preamble leaked: %q", got)
	}
	if !strings.Contains(got, "two problems block approval") {
		t.Fatalf("human-facing summary dropped: %q", got)
	}

	plain := "just prose, no structure"
	if got := StripVerdictTail(plain); got != plain {
		t.Fatalf("verdict-less answer must pass through unchanged: %q", got)
	}
}

func TestAugmentFromAnswer_StagesReview(t *testing.T) {
	cfg := Config{
		ExternalWorker: true,
		ReadOnly:       true,                                                                                // a code-reviewer is read-only
		IsReviewer:     true,                                                                                // structural signal (#482): the node's agent IS the code-reviewer
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

// TestAugmentFromAnswer_StagesReview_BareTaskText pins #482: the label-review
// default (dag.autoReviewTask, "Review this pull request.") has no posting verb
// at all, so the old task-text gate (demandsPostedReview) left this path dead.
// The structural signal (IsReviewer) stages it regardless of wording.
func TestAugmentFromAnswer_StagesReview_BareTaskText(t *testing.T) {
	cfg := Config{
		ExternalWorker: true,
		ReadOnly:       true,
		IsReviewer:     true,
		Setup:          &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"},
		Task:           "Review this pull request.",
	}
	act := workerActivity{}
	augmentFromAnswer(&act, cfg, reviewAnswer)
	if _, ok := act.stagedDelivery["review"]; !ok {
		t.Fatal("review not staged for a reviewer node with a bare, verb-less review task (#482)")
	}
}

func TestAugmentFromAnswer_Guards(t *testing.T) {
	reviewTask := "Review PR #7 and post your findings as inline review comments"
	reviewerCfg := Config{ExternalWorker: true, ReadOnly: true, IsReviewer: true,
		Setup: &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"}, Task: reviewTask}

	// Native workers stage via the tool — the probe must not fire.
	act := workerActivity{}
	cfg := reviewerCfg
	cfg.ExternalWorker = false
	augmentFromAnswer(&act, cfg, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe fired for a native worker")
	}

	// A non-reviewer node (IsReviewer false) must not stage a review, no matter
	// what its task says.
	act = workerActivity{}
	augmentFromAnswer(&act, Config{ExternalWorker: true, Task: "Add a widget, commit, and open a pull request"}, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe fired for a non-reviewer node")
	}

	// #471: a NON-read-only implementer whose task merely TALKS about reviews must
	// NOT stage a review — that review would ride alongside its PR and 404
	// against the trigger issue number. Now guaranteed structurally: an
	// implementer node is never IsReviewer, whatever its task says.
	act = workerActivity{}
	augmentFromAnswer(&act, Config{ExternalWorker: true, ReadOnly: false,
		Task: "Implement the HITL review flow: change how quack posts a review and open a pull request"}, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe staged a review for a non-reviewer implementer whose task mentions reviews (#471)")
	}

	// An already-staged review always wins (reviewer path).
	act = workerActivity{stagedDelivery: map[string]StagedDelivery{"review": {Kind: "review", Event: "approve", Body: "existing"}}}
	augmentFromAnswer(&act, reviewerCfg, reviewAnswer)
	if act.stagedDelivery["review"].Body != "existing" {
		t.Fatal("probe replaced a staged review")
	}

	// #482: a read-only reviewer node with NO Setup (a code-explorer investigating
	// the review path on an ISSUE, not a PR) must NOT stage a review — delivery
	// would then fail with "'' is not a github.com clone URL".
	act = workerActivity{}
	cfg = reviewerCfg
	cfg.Setup = nil
	augmentFromAnswer(&act, cfg, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe staged a review with no Setup (no PR to review against) (#482)")
	}
}
