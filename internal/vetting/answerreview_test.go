package vetting

import (
	"bytes"
	"log/slog"
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
	r := ParseAnswerReviewSections(reviewAnswer)
	if !r.OK || r.Event != "request_changes" {
		t.Fatalf("verdict: ok=%v event=%q", r.OK, r.Event)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("findings: got %d, want 2: %+v", len(r.Findings), r.Findings)
	}
	if r.Findings[0].Path != "internal/server/router.go" || r.Findings[0].Line != 42 || !strings.Contains(r.Findings[0].Body, "SPA fallback") {
		t.Fatalf("first finding wrong: %+v", r.Findings[0])
	}
	if r := ParseAnswerReviewSections("just prose, no structure"); r.OK {
		t.Fatal("prose must not parse as a verdict")
	}
}

func TestAugmentFromAnswer_StagesReview(t *testing.T) {
	cfg := Config{
		ExternalWorker: true,
		ReadOnly:       true,                                                                                // a code-reviewer is read-only
		IsReviewer:     true,                                                                                // the node's agent is the code-reviewer
		Setup:          &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"}, // a reviewer is setup-provisioned
		Task:           "Review PR #7 and post your findings as inline review comments",
	}
	act := workerActivity{}
	augmentFromAnswer(&act, cfg, reviewAnswer)
	st, ok := act.stagedDelivery["review"]
	if !ok {
		t.Fatal("review not staged")
	}
	if st.Event != "request_changes" || len(st.Comments) != 2 || !strings.Contains(st.Body, "Verdict: request changes") {
		t.Fatalf("staged review wrong: event=%q comments=%d body=%q", st.Event, len(st.Comments), st.Body)
	}
	if !st.Recovered {
		t.Error("a review staged by augmentFromAnswer must be marked Recovered (#688)")
	}

}

// An ACP reviewer that ended on prose with nothing staged must not count as a posted review.
func TestAugmentFromAnswer_ProseIsNotAReview(t *testing.T) {
	cfg := Config{ExternalWorker: true, ReadOnly: true, IsReviewer: true,
		Setup: &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"},
		Task:  "Review this pull request and post your findings as inline review comments and a verdict."}
	prose := "Analysis complete. My findings:\n- all call sites thread the correct root\n\nLet me stage the review and the durable record."
	act := workerActivity{}
	augmentFromAnswer(&act, cfg, prose)
	if st, ok := act.stagedDelivery["review"]; ok {
		t.Fatalf("prose with no VERDICT tail must not stage a review, got %+v", st)
	}
	if !workIncomplete(prose, cfg.Task, act, cfg.ReadOnly, false, cfg.IsReviewer, false) {
		t.Fatal("a reviewer with nothing staged must get a continuation round")
	}
	det := incompleteCriteria(cfg.Task, act, cfg.ReadOnly, false, cfg.IsReviewer, false)
	if det["review_posted"].Score != 0 {
		t.Fatalf("review_posted = %+v, want 0", det["review_posted"])
	}
	// The judge passing every rubric criterion must not pass the round.
	v := mergeDeterministic(verdict{Criteria: map[string]criterionScore{"structured_verdict": {Score: 1}, "catches_real_issues": {Score: 1}}}, det, cfg)
	if v.Score != 0 {
		t.Fatalf("round score = %v with no staged verdict, want 0", v.Score)
	}
}

// A synthesized-fanout slice has no stage_review tool: its prose still stages a comment review.
func TestAugmentFromAnswer_SliceKeepsCommentFallback(t *testing.T) {
	planID := "plan-slice-prose"
	ResetReviewFanout(planID)
	defer ResetReviewFanout(planID)
	fo := GetReviewFanout(planID, 2)
	fo.ExpectSynthesis()
	cfg := Config{ExternalWorker: true, ReadOnly: true, IsReviewer: true, ReviewFanout: fo,
		Setup: &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"},
		Task:  "YOUR SLICE - review ONLY these files"}
	act := workerActivity{}
	augmentFromAnswer(&act, cfg, "Slice is clean; nothing to stage.")
	if st := act.stagedDelivery["review"]; st.Event != "comment" {
		t.Fatalf("slice prose should stage a comment review, got %+v", st)
	}
}

// Taking over the review means the staging tools didn't run this round, which must log at Warn.
func TestAugmentFromAnswer_WarnsLoudly(t *testing.T) {
	cfg := Config{
		ExternalWorker: true, ReadOnly: true, IsReviewer: true, NodeID: "review-node-1",
		Setup: &SetupBranch{Repo: "https://github.com/fagerbergj/quack", WorkBranch: "quack/work"},
		Task:  "Review PR #7 and post your findings as inline review comments",
	}
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	act := workerActivity{}
	augmentFromAnswer(&act, cfg, reviewAnswer)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a WARN-level log when the tail fallback takes over, got: %s", out)
	}
	if !strings.Contains(out, "review-node-1") {
		t.Errorf("expected the log to name the node, got: %s", out)
	}
}

// The default label-review task has no posting verb; the structural IsReviewer signal stages the review
// regardless of wording.
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

	// Native workers stage via the tool - the probe must not fire.
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

	// An implementer whose task merely talks about reviews must not stage one (it would 404 against the
	// trigger issue): implementers are never IsReviewer.
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

	// A read-only reviewer with no Setup (an explorer on an issue, not a PR) must not stage a review:
	// delivery would fail with "'' is not a github.com clone URL".
	act = workerActivity{}
	cfg = reviewerCfg
	cfg.Setup = nil
	augmentFromAnswer(&act, cfg, reviewAnswer)
	if len(act.stagedDelivery) != 0 {
		t.Fatal("probe staged a review with no Setup (no PR to review against) (#482)")
	}
}

const sectionedReviewAnswer = `VERDICT: request_changes
FINDINGS:
- internal/server/router.go:42: the new route is registered after the SPA fallback, so it can never match
- frontend/src/state/chatStore.ts:118: optimistic write is not rolled back on a 409
DISMISSED:
- internal/vetting/node.go:12: looked risky but the retry already covers it
CLEAN:
- internal/store/artifact.go
- internal/recordstore/recordstore.go
`

func TestParseAnswerReviewSections(t *testing.T) {
	r := ParseAnswerReviewSections(sectionedReviewAnswer)
	if !r.OK || r.Event != "request_changes" {
		t.Fatalf("verdict: ok=%v event=%q", r.OK, r.Event)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("findings: got %d, want 2: %+v", len(r.Findings), r.Findings)
	}
	if len(r.Dismissed) != 1 || r.Dismissed[0].Path != "internal/vetting/node.go" || r.Dismissed[0].Line != 12 {
		t.Fatalf("dismissed: got %+v", r.Dismissed)
	}
	if len(r.Clean) != 2 || r.Clean[0] != "internal/store/artifact.go" || r.Clean[1] != "internal/recordstore/recordstore.go" {
		t.Fatalf("clean: got %+v", r.Clean)
	}
}

// A DISMISSED: line must not bleed into FINDINGS when sections are present.
func TestParseAnswerReviewSections_DismissedNotAbsorbedIntoFindings(t *testing.T) {
	r := ParseAnswerReviewSections(sectionedReviewAnswer)
	for _, f := range r.Findings {
		if f.Path == "internal/vetting/node.go" {
			t.Fatalf("DISMISSED entry leaked into Findings: %+v", f)
		}
	}
}

// Unstructured answers (no section headers) keep the unscoped fallback.
func TestParseAnswerReviewSections_FallbackWhenNoHeaders(t *testing.T) {
	r := ParseAnswerReviewSections(reviewAnswer)
	if !r.OK || len(r.Findings) != 2 {
		t.Fatalf("fallback scan: ok=%v findings=%d", r.OK, len(r.Findings))
	}
	if len(r.Dismissed) != 0 || len(r.Clean) != 0 {
		t.Fatalf("no DISMISSED/CLEAN headers should yield none: dismissed=%+v clean=%+v", r.Dismissed, r.Clean)
	}
}
