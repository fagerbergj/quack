package ledger

import (
	"context"
	"testing"
	"time"
)

func appendAt(t *testing.T, s LedgerStore, chatID string, at time.Time) {
	t.Helper()
	if _, err := s.AppendIntent(context.Background(), Entry{ChatID: chatID, Kind: KindNodeStarted, At: at}); err != nil {
		t.Fatalf("AppendIntent(%s): %v", chatID, err)
	}
}

// TestSweepOnceDeletesOnlyExpired: an old session is deleted, a fresh one kept.
func TestSweepOnceDeletesOnlyExpired(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	appendAt(t, s, "old", time.Now().Add(-40*24*time.Hour))
	appendAt(t, s, "fresh", time.Now())

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
	s := NewMemStore()
	ctx := context.Background()
	appendAt(t, s, "ancient", time.Now().Add(-3650*24*time.Hour))

	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	RunRetentionSweep(runCtx, s, 0, 50*time.Millisecond)

	if refs, _ := s.List(ctx); len(refs) != 1 {
		t.Fatalf("retention_days=0 deleted a session; got %+v, want it kept forever", refs)
	}
}

// TestRunRetentionSweepRunsImmediatelyThenTicks: the initial sweep fires
// without waiting for the first tick.
func TestRunRetentionSweepRunsImmediatelyThenTicks(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	appendAt(t, s, "old", time.Now().Add(-40*24*time.Hour))

	// A tick far longer than the deadline: deletion must come from the immediate sweep.
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	RunRetentionSweep(runCtx, s, 30, time.Hour)

	if refs, _ := s.List(ctx); len(refs) != 0 {
		t.Fatalf("expected immediate sweep to delete the old session; got %+v", refs)
	}
}
