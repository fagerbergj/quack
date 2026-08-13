package memory

import (
	"context"
	"testing"
	"time"
)

// seedMemory upserts one point with the lifecycle/provenance fields the
// consolidation sweep reads - upsertScoped (store_test.go) only sets
// content/scope/author, not enough to exercise clustering or retention.
func seedMemory(t *testing.T, s *Store, p point) {
	t.Helper()
	if p.Vector == nil {
		p.Vector = []float32{1, 0, 0, 0}
	}
	if err := s.idx.upsert(context.Background(), []point{p}); err != nil {
		t.Fatalf("seed upsert %s: %v", p.ID, err)
	}
}

// TestConsolidateOnce_BurstDedupe covers design doc §7 case 4: 3 unverified
// memories from one chat_id, minted within the clustering window, near-
// identical claims. The (faked) consolidation model keeps one via UPDATE and
// deletes the other two, reason "duplicate of <id>" - decide()/apply()'s
// normal op taxonomy, applied from the sweep's ticker trigger instead of a
// commit.
func TestConsolidateOnce_BurstDedupe(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	seedMemory(t, s, point{ID: "m1", Content: "the build command is make build", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m2", Content: "run make build to build the project", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m3", Content: "the project builds via make build", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:10:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})

	s.consolidator = fakeModel{reply: `{"ops":[
		{"action":"UPDATE","id":"m1","content":"the build command is make build","kind":"command"},
		{"action":"DELETE","id":"m2","reason":"duplicate of m1"},
		{"action":"DELETE","id":"m3","reason":"duplicate of m1"}
	]}`}

	s.consolidateOnce(ctx)

	valid, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(valid) != 1 || valid[0].ID != "m1" {
		t.Fatalf("valid memories = %+v, want exactly [m1]", valid)
	}

	all, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("List(includeInvalidated): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all memories = %+v, want 3 (invalidate never removes)", all)
	}
	byID := map[string]Memory{}
	for _, m := range all {
		byID[m.ID] = m
	}
	if byID["m2"].Status != string(StatusInvalidated) || byID["m2"].InvalidationReason != "duplicate of m1" {
		t.Fatalf("m2 = %+v, want status=invalidated reason=%q", byID["m2"], "duplicate of m1")
	}
	if byID["m3"].Status != string(StatusInvalidated) || byID["m3"].InvalidationReason != "duplicate of m1" {
		t.Fatalf("m3 = %+v, want status=invalidated reason=%q", byID["m3"], "duplicate of m1")
	}

	if len(ops.rows) != 3 {
		t.Fatalf("ops rows = %+v, want 3 (1 update + 2 invalidate)", ops.rows)
	}
	for _, r := range ops.rows {
		if r.actor != ActorConsolidator {
			t.Fatalf("op row %+v, want actor=consolidator", r)
		}
	}
}

// TestBurstClusters_DifferentChatsDoNotCluster covers design doc §7 case 4's
// negative: two near-identical memories from different chat_ids never join a
// cluster, even minted at the same instant.
func TestBurstClusters_DifferentChatsDoNotCluster(t *testing.T) {
	pts := []scored{
		{ID: "a", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z"},
		{ID: "b", ChatID: "chat-2", MintedAt: "2026-08-13T00:00:00Z"},
	}
	if got := burstClusters(pts); len(got) != 0 {
		t.Fatalf("burstClusters across chats = %+v, want none", got)
	}
}

// TestBurstClusters_FarApartMintedAtDoNotCluster covers the other negative:
// same chat_id but outside the clustering window never joins.
func TestBurstClusters_FarApartMintedAtDoNotCluster(t *testing.T) {
	pts := []scored{
		{ID: "a", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z"},
		{ID: "b", ChatID: "chat-1", MintedAt: "2026-08-13T01:00:00Z"}, // 1h later, past the 15m window
	}
	if got := burstClusters(pts); len(got) != 0 {
		t.Fatalf("burstClusters far apart = %+v, want none", got)
	}
}

// TestBurstClusters_WithinWindowChains covers the positive shape burstClusters
// feeds consolidateOnce: same chat_id, each gap <= the window, chains into one
// cluster even across more than two hops.
func TestBurstClusters_WithinWindowChains(t *testing.T) {
	pts := []scored{
		{ID: "a", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z"},
		{ID: "b", ChatID: "chat-1", MintedAt: "2026-08-13T00:10:00Z"},
		{ID: "c", ChatID: "chat-1", MintedAt: "2026-08-13T00:20:00Z"},
	}
	got := burstClusters(pts)
	if len(got) != 1 || len(got[0]) != 3 {
		t.Fatalf("burstClusters = %+v, want one cluster of 3", got)
	}
}

// TestConsolidateOnce_SkipsReinforcedAndInvalidatedNeighbours covers design
// doc §7's implicit rule: a reinforced memory (earned trust) and an already-
// invalidated one, both sharing the dedupe-eligible pair's chat_id/time
// window, never enter the candidate set - neither is named in an op, and
// both survive with their status unchanged.
func TestConsolidateOnce_SkipsReinforcedAndInvalidatedNeighbours(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	seedMemory(t, s, point{ID: "m1", Content: "dup A", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z", Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m2", Content: "dup B", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z", Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m3", Content: "already proven", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-1", MintedAt: "2026-08-13T00:08:00Z", Status: string(StatusReinforced), ValidFrom: "t", ReinforcementCount: 2})
	seedMemory(t, s, point{ID: "m4", Content: "already retracted", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-1", MintedAt: "2026-08-13T00:09:00Z", Status: string(StatusInvalidated), ValidFrom: "t",
		InvalidatedAt: "t", InvalidationReason: "prior sweep"})

	s.consolidator = fakeModel{reply: `{"ops":[
		{"action":"UPDATE","id":"m1","content":"dup A","kind":"convention"},
		{"action":"DELETE","id":"m2","reason":"duplicate of m1"}
	]}`}

	s.consolidateOnce(ctx)

	all, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Memory{}
	for _, m := range all {
		byID[m.ID] = m
	}
	if byID["m3"].Status != string(StatusReinforced) || byID["m3"].ReinforcementCount != 2 {
		t.Fatalf("m3 (reinforced) = %+v, want unchanged", byID["m3"])
	}
	if byID["m4"].Status != string(StatusInvalidated) || byID["m4"].InvalidationReason != "prior sweep" {
		t.Fatalf("m4 (already invalidated) = %+v, want unchanged", byID["m4"])
	}
	for _, r := range ops.rows {
		if r.memoryID == "m3" || r.memoryID == "m4" {
			t.Fatalf("ops rows = %+v, m3/m4 must never be touched", ops.rows)
		}
	}
}

// TestRetentionOnce_RemovesExpiredKeepsFreshAndValid covers design doc §6: an
// invalidated point older than retentionDays is hard-removed, a recently
// invalidated one survives, and a currently-valid point is never a candidate
// regardless of age.
func TestRetentionOnce_RemovesExpiredKeepsFreshAndValid(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	fresh := now.Add(-1 * 24 * time.Hour).Format(time.RFC3339)

	seedMemory(t, s, point{ID: "expired", Content: "long invalidated", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: old, InvalidationReason: "stale"})
	seedMemory(t, s, point{ID: "recent", Content: "recently invalidated", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: fresh, InvalidationReason: "stale"})
	seedMemory(t, s, point{ID: "valid", Content: "still valid", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusUnverified), ValidFrom: "t"})

	s.retentionOnce(ctx, 30)

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range remaining {
		ids[m.ID] = true
	}
	if ids["expired"] {
		t.Fatalf("remaining = %+v, want expired hard-removed", remaining)
	}
	if !ids["recent"] || !ids["valid"] {
		t.Fatalf("remaining = %+v, want recent and valid kept", remaining)
	}

	if len(ops.pruneCalls) != 1 {
		t.Fatalf("PruneMemoryOps calls = %d, want 1", len(ops.pruneCalls))
	}
}

// TestRetentionOnce_ZeroRetentionRemovesNothing covers design doc §6's
// explicit-not-implicit default: retention_days=0 must never delete a point
// or prune memory_ops, no matter how old.
func TestRetentionOnce_ZeroRetentionRemovesNothing(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	ancient := time.Now().Add(-3650 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "ancient", Content: "very old", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: ancient, InvalidationReason: "stale"})

	s.retentionOnce(ctx, 0)

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("retention_days=0 removed a point; remaining = %+v, want 1", remaining)
	}
	if len(ops.pruneCalls) != 0 {
		t.Fatalf("PruneMemoryOps calls = %d, want 0 (true no-op)", len(ops.pruneCalls))
	}
}

// TestRunConsolidationSweep_IntervalZeroIsNoop covers the config's
// 0/absent-disables convention (matching ledger.RunRetentionSweep): interval
// <= 0 returns immediately without touching the store, even when retention
// would otherwise have work to do.
func TestRunConsolidationSweep_IntervalZeroIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	ancient := time.Now().Add(-3650 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "ancient", Content: "very old", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: ancient, InvalidationReason: "stale"})

	s.RunConsolidationSweep(ctx, 0, 30) // synchronous: a real sweep would block on the ticker loop

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("interval=0 did work; remaining = %+v, want untouched", remaining)
	}
	if len(ops.pruneCalls) != 0 {
		t.Fatalf("PruneMemoryOps calls = %d, want 0", len(ops.pruneCalls))
	}
}

// TestRunConsolidationSweep_RunsImmediatelyThenTicks mirrors
// ledger.TestRunRetentionSweepRunsImmediatelyThenTicks: the first sweep fires
// before the first tick, so a tick interval far longer than the test's own
// deadline still observes the retention work done.
func TestRunConsolidationSweep_RunsImmediatelyThenTicks(t *testing.T) {
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "old", Content: "expired", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: old, InvalidationReason: "stale"})

	runCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.RunConsolidationSweep(runCtx, time.Hour, 30)

	remaining, _, err := s.List(context.Background(), []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected the immediate sweep to remove the expired point; got %+v", remaining)
	}
}
