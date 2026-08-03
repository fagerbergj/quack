package vetting

import "testing"

// TestAugmentFromReviewStage_ToolStagedWins proves the gate reads a tool-staged
// review (the #451 review MCP surface, resolved via advisor token → MemSecret →
// MemSession.Review) into act.stagedDelivery["review"], and that the answer-tail
// fallback (augmentFromAnswer) does NOT overwrite it - the two never conflict.
func TestAugmentFromReviewStage_ToolStagedWins(t *testing.T) {
	secret, err := NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	review := &ReviewStage{}
	review.AddComment("internal/foo.go", 12, "blocking: nil deref on the empty path")
	review.SetVerdict("approve", "one nit, nothing blocking")
	RegisterMemSession(secret, MemSession{Review: review})
	defer UnregisterMemSession(secret)

	token := AdvisorThreadToken("plan-1", "node-1")
	RegisterAdvisorThread(token, AdvisorTask{NodeID: "node-1", MemSecret: secret})
	defer UnregisterAdvisorThread(token)

	cfg := Config{ExternalWorker: true, Task: "Review PR #7 and post your findings as inline review comments"}
	act := workerActivity{}
	augmentFromReviewStage(&act, token)
	// reviewAnswer's tail carries request_changes + 2 findings; it must lose to
	// the tool-staged approve + 1 finding.
	augmentFromAnswer(&act, cfg, reviewAnswer)

	st, ok := act.stagedDelivery["review"]
	if !ok {
		t.Fatal("review not staged from the tool buffer")
	}
	if st.Event != "approve" || st.Body != "one nit, nothing blocking" {
		t.Fatalf("answer tail overwrote the tool-staged review: %+v", st)
	}
	if len(st.Comments) != 1 || st.Comments[0].Path != "internal/foo.go" || st.Comments[0].Line != 12 {
		t.Fatalf("tool-staged inline comments lost: %+v", st.Comments)
	}
}

// TestReviewStage_SnapshotVerdictless proves comments without an explicit
// verdict still produce a deliverable review, defaulting to a comment event -
// mirroring augmentFromAnswer's verdict-less fallback so a reviewer that only
// stages inline findings never deadlocks the node.
func TestReviewStage_SnapshotVerdictless(t *testing.T) {
	review := &ReviewStage{}
	if _, ok := review.Snapshot(); ok {
		t.Fatal("an empty buffer must not snapshot as a staged review")
	}
	review.AddComment("a.go", 3, "nit: rename")
	sd, ok := review.Snapshot()
	if !ok {
		t.Fatal("a staged comment should produce a deliverable review")
	}
	if sd.Event != "comment" || len(sd.Comments) != 1 {
		t.Fatalf("verdict-less snapshot wrong: %+v", sd)
	}
}

// TestReviewStage_RemoveComment proves unstage_review_comment's backing store
// (#562): an exact (path, line, body) match is removed, a same-line
// different-body finding survives untouched, and removing something that was
// never staged (or already removed) is a harmless no-op rather than an error.
func TestReviewStage_RemoveComment(t *testing.T) {
	review := &ReviewStage{}
	review.AddComment("a.go", 3, "nit: rename x")
	review.AddComment("a.go", 3, "blocking: unrelated finding, same line")
	review.AddComment("b.go", 9, "suggestion: extract helper")

	// No-op: nothing staged at this path/line/body.
	review.RemoveComment("z.go", 1, "never staged")
	sd, ok := review.Snapshot()
	if !ok || len(sd.Comments) != 3 {
		t.Fatalf("no-op remove must not touch the buffer: %+v", sd.Comments)
	}

	review.RemoveComment("a.go", 3, "nit: rename x")
	sd, ok = review.Snapshot()
	if !ok || len(sd.Comments) != 2 {
		t.Fatalf("want 2 comments after removing one, got %+v", sd.Comments)
	}
	for _, c := range sd.Comments {
		if c.Path == "a.go" && c.Body == "nit: rename x" {
			t.Fatalf("removed comment still present: %+v", sd.Comments)
		}
	}
	// The same-line, different-body finding must survive.
	found := false
	for _, c := range sd.Comments {
		if c.Path == "a.go" && c.Line == 3 && c.Body == "blocking: unrelated finding, same line" {
			found = true
		}
	}
	if !found {
		t.Fatalf("same-line different-body finding was wrongly dropped: %+v", sd.Comments)
	}

	// Retracting twice is harmless, not an error.
	review.RemoveComment("a.go", 3, "nit: rename x")
	if sd, _ := review.Snapshot(); len(sd.Comments) != 2 {
		t.Fatalf("double-retract must be a no-op, got %+v", sd.Comments)
	}
}
