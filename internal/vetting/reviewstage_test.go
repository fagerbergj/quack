package vetting

import "testing"

// TestAugmentFromReviewStage_ToolStagedWins proves the gate reads a tool-staged
// review (the #451 review MCP surface, resolved via advisor token → MemSecret →
// MemSession.Review) into act.stagedDelivery["review"], and that the answer-tail
// fallback (augmentFromAnswer) does NOT overwrite it — the two never conflict.
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
// verdict still produce a deliverable review, defaulting to a comment event —
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
