package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestMemStoreAppendIntent_ParentRevisionConflict is the fast, no-docker
// mirror of TestPGStoreAppendIntent_ParentRevisionConflict (#1144 P4): the
// same contract, checked without a real Postgres container.
func TestMemStoreAppendIntent_ParentRevisionConflict(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	payload, _ := json.Marshal(struct {
		ParentRevision int `json:"parent_revision"`
	}{ParentRevision: 0})

	if _, err := s.AppendIntent(ctx, Entry{ChatID: "chat1", Kind: KindArtifactRevision, Key: "id1", Payload: payload}); err != nil {
		t.Fatalf("first AppendIntent: %v", err)
	}
	if _, err := s.AppendIntent(ctx, Entry{ChatID: "chat1", Kind: KindArtifactRevision, Key: "id1", Payload: payload}); !errors.Is(err, ErrStaleParent) {
		t.Fatalf("second AppendIntent for the same parent = %v, want ErrStaleParent", err)
	}
}

// TestMemStoreAppendIntent_IdempotencyKeyIsANoOp mirrors
// TestPGStoreAppendIntent_IdempotencyKeyIsANoOp without a container.
func TestMemStoreAppendIntent_IdempotencyKeyIsANoOp(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()

	seq1, err := s.AppendIntent(ctx, Entry{ChatID: "chat1", Kind: KindArtifactRevision, Key: "id1", IdempotencyKey: "dup"})
	if err != nil {
		t.Fatalf("first AppendIntent: %v", err)
	}
	_, err = s.AppendIntent(ctx, Entry{ChatID: "chat1", Kind: KindArtifactRevision, Key: "id1", IdempotencyKey: "dup"})
	var dup *DuplicateIntentError
	if !errors.As(err, &dup) {
		t.Fatalf("second AppendIntent = %v, want *DuplicateIntentError", err)
	}
	if dup.Existing.Seq != seq1 {
		t.Fatalf("DuplicateIntentError.Existing.Seq = %d, want %d", dup.Existing.Seq, seq1)
	}
}
