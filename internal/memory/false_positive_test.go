package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestFalsePositiveCorrectionRecalledByReviewer is the acceptance path for
// #249: a conversational correction, committed into coding memory with the
// same repo+role scope as the former correct_review_finding tool used (repo
// bucket + coding role), is recalled through the SAME bucket the gate's
// Recall reads for a later review of that repo (memoryScope in
// internal/vetting/node.go; codingView above mirrors it).
func TestFalsePositiveCorrectionRecalledByReviewer(t *testing.T) {
	ctx := context.Background()
	const correction = `False positive on acme/games PR #246: "empty Comment.Body breaks dispatch via triggerTask" was flagged in review but is NOT a real issue - dispatch takes the task string directly, it never calls triggerTask`
	// addOp naively string-embeds its content into a JSON literal; the
	// correction's own quotes/colons would break that, so build the reply with
	// a real encoder instead.
	reply, err := json.Marshal(struct {
		Ops []op `json:"ops"`
	}{Ops: []op{{Action: "ADD", Content: correction, Kind: "false_positive"}}})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	s := newSQLiteStore(t, "task", fakeModel{reply: string(reply)})

	sc := Scope{Repo: repoA, Role: RoleCoding}
	if _, err := s.Commit(ctx, sc, "orchestrator",
		[]Candidate{{Content: correction, Metadata: map[string]string{"kind": "false_positive"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := recall(t, codingView(s, "code-reviewer", repoA, "u1"), "does an empty Comment.Body break dispatch")
	if len(got) != 1 {
		t.Fatalf("code-reviewer recalled %d memories, want 1 (the correction must reach its next review)", len(got))
	}
	if text := extractText(got[0]); !strings.Contains(text, "NOT a real issue") {
		t.Fatalf("recalled memory = %q, missing the correction", text)
	}

	// A different repo's reviewer must not see it (the bucket is the repo).
	if got := recall(t, codingView(s, "code-reviewer", repoB, "u1"), "does an empty Comment.Body break dispatch"); len(got) != 0 {
		t.Fatalf("other repo recalled %d memories, want 0 (repo buckets must not bleed)", len(got))
	}
}
