package ledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSweepOnceDeletesOnlyExpired: an old session is deleted, a fresh one
// kept.
func TestSweepOnceDeletesOnlyExpired(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := s.Append(ctx, "old", []byte(`{}`)); err != nil {
		t.Fatalf("Append(old): %v", err)
	}
	if err := s.Append(ctx, "fresh", []byte(`{}`)); err != nil {
		t.Fatalf("Append(fresh): %v", err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old.jsonl"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	sweepOnce(ctx, s, 30)

	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != "fresh" {
		t.Fatalf("got %+v, want only session fresh", refs)
	}
}

// TestRunRetentionSweepZeroMeansForever: retentionDays 0 must never delete.
func TestRunRetentionSweepZeroMeansForever(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := s.Append(ctx, "ancient", []byte(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ancient := time.Now().Add(-3650 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "ancient.jsonl"), ancient, ancient); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	RunRetentionSweep(runCtx, s, 0, 50*time.Millisecond)

	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("retention_days=0 deleted a session; got %+v, want it kept forever", refs)
	}
}

// TestRunRetentionSweepRunsImmediatelyThenTicks: the initial sweep fires
// without waiting for the first tick.
func TestRunRetentionSweepRunsImmediatelyThenTicks(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := s.Append(ctx, "old", []byte(`{}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old.jsonl"), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// A tick interval far longer than the test's own deadline: if deletion
	// only happened on the ticker.C branch, this would time out with the
	// session still present.
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	RunRetentionSweep(runCtx, s, 30, time.Hour)

	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected immediate sweep to delete the old session; got %+v", refs)
	}
}
