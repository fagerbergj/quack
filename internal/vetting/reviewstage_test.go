package vetting

import "testing"

// TestAugmentFromReviewStage_ToolStagedWins: a tool-staged review (via advisor token → MemSession.Review)
// lands in act.stagedDelivery["review"], and the answer-tail fallback does NOT overwrite it.
func TestAugmentFromReviewStage_ToolStagedWins(t *testing.T) {
	secret, err := NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	review := &ReviewStage{}
	review.AddComment("internal/foo.go", 12, "blocking: nil deref on the empty path")
	review.SetVerdict("approve", "one nit, nothing blocking", nil, nil)
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
	if st.Event != "approve" || st.Takeaway != "one nit, nothing blocking" {
		t.Fatalf("answer tail overwrote the tool-staged review: %+v", st)
	}
	if len(st.Comments) != 1 || st.Comments[0].Path != "internal/foo.go" || st.Comments[0].Line != 12 {
		t.Fatalf("tool-staged inline comments lost: %+v", st.Comments)
	}
	if st.Recovered {
		t.Fatal("a tool-staged review must not be marked Recovered (#688)")
	}
}

// TestReviewStage_SnapshotVerdictless: comments with no explicit verdict still deliver as a comment
// event, so a reviewer that only stages inline findings never deadlocks the node.
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

// TestReviewStage_RemoveComment: removal is by id, a same-line different-body finding survives,
// and an unknown id reports ok=false so the caller can surface an error.
func TestReviewStage_RemoveComment(t *testing.T) {
	review := &ReviewStage{}
	id1, _ := review.AddComment("a.go", 3, "nit: rename x")
	review.AddComment("a.go", 3, "blocking: unrelated finding, same line")
	review.AddComment("b.go", 9, "suggestion: extract helper")

	// Unknown id: ok=false, buffer untouched.
	if ok := review.RemoveComment("z.go:1#1"); ok {
		t.Fatal("removing an unknown id must report ok=false")
	}
	sd, ok := review.Snapshot()
	if !ok || len(sd.Comments) != 3 {
		t.Fatalf("failed remove must not touch the buffer: %+v", sd.Comments)
	}

	if ok := review.RemoveComment(id1); !ok {
		t.Fatal("removing a known id must report ok=true")
	}
	sd, ok = review.Snapshot()
	if !ok || len(sd.Comments) != 2 {
		t.Fatalf("want 2 comments after removing one, got %+v", sd.Comments)
	}
	for _, c := range sd.Comments {
		if c.Body == "nit: rename x" {
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

	// Retracting the same id twice is an error, not silently accepted.
	if ok := review.RemoveComment(id1); ok {
		t.Fatal("double-retract of the same id must report ok=false")
	}
	if sd, _ := review.Snapshot(); len(sd.Comments) != 2 {
		t.Fatalf("double-retract must not change the buffer, got %+v", sd.Comments)
	}
}

// TestReviewStage_IDsMonotonicPerLocation: #n ids are monotonic per (path, line) and never reused, so a
// stale reference can't silently resolve to a different comment after an unstage/restage.
func TestReviewStage_IDsMonotonicPerLocation(t *testing.T) {
	review := &ReviewStage{}
	id1, _ := review.AddComment("a.go", 3, "first finding at this line")
	if id1 != "a.go:3#1" {
		t.Fatalf("first id = %q, want \"a.go:3#1\"", id1)
	}
	if ok := review.RemoveComment(id1); !ok {
		t.Fatalf("removing %q should succeed", id1)
	}

	id2, _ := review.AddComment("a.go", 3, "second, different finding at the same line")
	if id2 != "a.go:3#2" {
		t.Fatalf("re-staged id = %q, want \"a.go:3#2\" (monotonic, not reused)", id2)
	}
	if id2 == id1 {
		t.Fatal("re-staged id must not equal the retracted one")
	}

	// The old id must stay unresolvable, not accidentally match the new comment.
	if ok := review.RemoveComment(id1); ok {
		t.Fatalf("stale id %q must not resolve to the new comment %q", id1, id2)
	}
	sd, ok := review.Snapshot()
	if !ok || len(sd.Comments) != 1 || sd.Comments[0].Body != "second, different finding at the same line" {
		t.Fatalf("expected only the re-staged comment to remain: %+v", sd.Comments)
	}
}

// TestReviewStage_AddCommentDedupes: staging the identical path/line/body twice yields one comment;
// the second call reports duplicate and returns the first id, so delivery never double-posts.
func TestReviewStage_AddCommentDedupes(t *testing.T) {
	review := &ReviewStage{}
	id1, dup1 := review.AddComment("a.go", 3, "blocking: nil deref")
	if dup1 {
		t.Fatal("first staging of a finding must not report a duplicate")
	}
	id2, dup2 := review.AddComment("a.go", 3, "blocking: nil deref")
	if !dup2 {
		t.Fatal("re-staging the same path/line/body must report a duplicate")
	}
	if id2 != id1 {
		t.Fatalf("duplicate id = %q, want the existing id %q", id2, id1)
	}
	sd, ok := review.Snapshot()
	if !ok || len(sd.Comments) != 1 {
		t.Fatalf("want exactly 1 staged comment after a duplicate call, got %+v", sd.Comments)
	}

	// A different body at the same line is a distinct finding, not a duplicate.
	id3, dup3 := review.AddComment("a.go", 3, "nit: rename")
	if dup3 {
		t.Fatal("a different body at the same path/line must not be treated as a duplicate")
	}
	if id3 == id1 {
		t.Fatal("a distinct finding must get its own id")
	}
}
