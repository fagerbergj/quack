package memory

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// seedMemory sets the lifecycle/provenance fields the sweep reads, which upsertScoped does not.
func seedMemory(t *testing.T, s *Store, p point) {
	t.Helper()
	if p.Vector == nil {
		p.Vector = []float32{1, 0, 0, 0}
	}
	if err := s.idx.upsert(context.Background(), []point{p}); err != nil {
		t.Fatalf("seed upsert %s: %v", p.ID, err)
	}
}

// errIndex forces remove() to fail over a real index, to pin the sweep's warn-and-continue path.
type errIndex struct {
	index
	removeErr error
}

func (e *errIndex) remove(ctx context.Context, ids []string) (int, error) {
	if e.removeErr != nil {
		return 0, e.removeErr
	}
	return e.index.remove(ctx, ids)
}

// Three near-identical unverified memories in one chat's window: the faked model UPDATEs one and DELETEs
// two as "duplicate of <id>", which is normalized to "absorbed by <id>" with absorbed_ids recording both.
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

	valid, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, false, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(valid) != 1 || valid[0].ID != "m1" {
		t.Fatalf("valid memories = %+v, want exactly [m1]", valid)
	}

	all, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
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
	if byID["m2"].Status != string(StatusInvalidated) || byID["m2"].InvalidationReason != "absorbed by m1" {
		t.Fatalf("m2 = %+v, want status=invalidated reason=%q", byID["m2"], "absorbed by m1")
	}
	if byID["m3"].Status != string(StatusInvalidated) || byID["m3"].InvalidationReason != "absorbed by m1" {
		t.Fatalf("m3 = %+v, want status=invalidated reason=%q", byID["m3"], "absorbed by m1")
	}
	sortedAbsorbed := append([]string(nil), byID["m1"].AbsorbedIDs...)
	sort.Strings(sortedAbsorbed)
	if !reflect.DeepEqual(sortedAbsorbed, []string{"m2", "m3"}) {
		t.Fatalf("m1 absorbed_ids = %v, want [m2 m3]", sortedAbsorbed)
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

// Near-identical memories from different chat_ids never cluster, even minted at the same instant.
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

// Same chat_id with every gap <= the window chains into one cluster across several hops.
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

// An unparsable MintedAt is dropped, not guessed, and the chain before it still flushes. The bad value
// sorts between b and c so it interrupts the chain rather than landing at an end.
func TestBurstClusters_UnparsableMintedAtFlushesAndSkips(t *testing.T) {
	pts := []scored{
		{ID: "a", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z"},
		{ID: "b", ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z"},
		{ID: "bad", ChatID: "chat-1", MintedAt: "2026-08-13T00:07:00"}, // no zone: fails time.Parse
		{ID: "c", ChatID: "chat-1", MintedAt: "2026-08-13T00:10:00Z"},
	}
	got := burstClusters(pts)
	if len(got) != 1 || len(got[0]) != 2 || got[0][0].ID != "a" || got[0][1].ID != "b" {
		t.Fatalf("burstClusters = %+v, want one flushed cluster [a b] (bad and lone c dropped)", got)
	}
}

// Reinforced and already-invalidated neighbours in the same window never enter the candidate set.
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

	all, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
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

// TestConsolidateCluster_AllNoopOpsWriteNothing: an all-NOOP reply calls the
// model but writes nothing - apply() only writes ADD/UPDATE/DELETE.
func TestConsolidateCluster_AllNoopOpsWriteNothing(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[{"action":"NOOP","id":"m1"},{"action":"NOOP","id":"m2"}]}`})

	n, err := s.consolidateCluster(ctx, "repo:r", []scored{
		{ID: "m1", Content: "fact A", ChatID: "chat-1"},
		{ID: "m2", Content: "fact B", ChatID: "chat-1"},
	})
	if err != nil {
		t.Fatalf("consolidateCluster: %v", err)
	}
	if n != 0 {
		t.Fatalf("writes = %d, want 0 (an all-NOOP reply changes nothing)", n)
	}
}

// TestConsolidateOnce_SkipsUnchangedClusterNextSweep: a burst judged a pure
// no-op is called once, then skipped until membership changes.
func TestConsolidateOnce_SkipsUnchangedClusterNextSweep(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	seedMemory(t, s, point{ID: "m1", Content: "the frontend uses vite", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m2", Content: "the backend is written in go", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})

	calls := 0
	s.consolidator = counting(&calls, `{"ops":[]}`)

	s.consolidateOnce(ctx) // burst is new: must call
	if calls != 1 {
		t.Fatalf("calls after first sweep = %d, want 1", calls)
	}

	s.consolidateOnce(ctx) // same burst, already judged no-op: must skip
	if calls != 1 {
		t.Fatalf("calls after second (unchanged) sweep = %d, want still 1 (skip)", calls)
	}

	// A new member joins the burst: the fingerprint changes, forcing a call.
	seedMemory(t, s, point{ID: "m3", Content: "tests run via make test", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:07:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})

	s.consolidateOnce(ctx)
	if calls != 2 {
		t.Fatalf("calls after a new member joined the burst = %d, want 2", calls)
	}
}

// The fingerprint hashes content, so editing a stamped member's wording forces a fresh call.
func TestConsolidateOnce_RecallsWhenMemberContentChanges(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	seedMemory(t, s, point{ID: "m1", Content: "the frontend uses vite", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m2", Content: "the backend is written in go", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})

	calls := 0
	s.consolidator = counting(&calls, `{"ops":[]}`)

	s.consolidateOnce(ctx)
	s.consolidateOnce(ctx) // confirm the skip engages before editing
	if calls != 1 {
		t.Fatalf("calls before the edit = %d, want 1", calls)
	}

	seedMemory(t, s, point{ID: "m1", Content: "the frontend uses vite 6", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})

	s.consolidateOnce(ctx)
	if calls != 2 {
		t.Fatalf("calls after editing m1's content = %d, want 2", calls)
	}
}

// The fingerprint hashes only id+content, so a vote-only change stays skipped.
func TestConsolidateOnce_StaysSkippedAfterVoteOnlyChange(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	seedMemory(t, s, point{ID: "m1", Content: "the frontend uses vite", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "m2", Content: "the backend is written in go", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z",
		Status: string(StatusUnverified), ValidFrom: "t"})

	calls := 0
	s.consolidator = counting(&calls, `{"ops":[]}`)

	s.consolidateOnce(ctx)
	if calls != 1 {
		t.Fatalf("calls after first sweep = %d, want 1", calls)
	}

	if _, err := s.ApplyVotes(ctx, []Vote{{MemoryID: "m1", Vote: VoteSupported, Actor: ActorJudge}}, DefaultInvalidateThreshold); err != nil {
		t.Fatalf("ApplyVotes: %v", err)
	}

	s.consolidateOnce(ctx)
	if calls != 1 {
		t.Fatalf("calls after a vote-only change = %d, want still 1 (skip)", calls)
	}
}

// An invalidated point older than retentionDays is removed, a recently invalidated one survives, and a
// valid point is never a candidate regardless of age.
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

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
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

// retention_days=0 never deletes a point or prunes memory_ops, however old.
func TestRetentionOnce_ZeroRetentionRemovesNothing(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	ancient := time.Now().Add(-3650 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "ancient", Content: "very old", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: ancient, InvalidationReason: "stale"})

	s.retentionOnce(ctx, 0)

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
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

// TestRunConsolidationSweep_ScheduleEmptyIsNoop: an empty schedule is a
// no-op, even when retention would otherwise have work to do.
func TestRunConsolidationSweep_ScheduleEmptyIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	ancient := time.Now().Add(-3650 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "ancient", Content: "very old", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: ancient, InvalidationReason: "stale"})

	s.RunConsolidationSweep(ctx, "", 30) // synchronous: a real sweep would block on the timer loop

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("schedule=\"\" did work; remaining = %+v, want untouched", remaining)
	}
	if len(ops.pruneCalls) != 0 {
		t.Fatalf("PruneMemoryOps calls = %d, want 0", len(ops.pruneCalls))
	}
}

// TestRunConsolidationSweep_InvalidScheduleIsNoop: a malformed cron (should
// never reach here past config validation) must fail closed, not panic.
func TestRunConsolidationSweep_InvalidScheduleIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	s.RunConsolidationSweep(ctx, "not a cron", 30) // must return, not panic or hang
}

// One cluster's malformed reply must not stop the sweep: both independent bursts get a call, and
// neither's points are touched.
func TestConsolidateOnce_ClusterErrorContinuesToNextCluster(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	seedMemory(t, s, point{ID: "a1", Content: "dup A1", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z", Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "a2", Content: "dup A2", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-1", MintedAt: "2026-08-13T00:05:00Z", Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "b1", Content: "dup B1", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-2", MintedAt: "2026-08-13T00:00:00Z", Status: string(StatusUnverified), ValidFrom: "t"})
	seedMemory(t, s, point{ID: "b2", Content: "dup B2", Scope: "repo:r", Author: "a", Timestamp: "t",
		ChatID: "chat-2", MintedAt: "2026-08-13T00:05:00Z", Status: string(StatusUnverified), ValidFrom: "t"})

	calls := 0
	s.consolidator = counting(&calls, "not valid json")

	s.consolidateOnce(ctx) // must not panic or stop after the first cluster's error

	if calls != 2 {
		t.Fatalf("consolidator calls = %d, want 2 (both clusters attempted despite the first failing)", calls)
	}
	all, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("all memories = %+v, want all 4 untouched (every decideDedupe call failed to parse)", all)
	}
	if len(ops.rows) != 0 {
		t.Fatalf("ops rows = %+v, want none (no op ever applied)", ops.rows)
	}
}

// idx.remove() failing must not stop retentionOnce from attempting the memory_ops prune.
func TestRetentionOnce_RemoveErrorContinuesToPrune(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "expired", Content: "long invalidated", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: old, InvalidationReason: "stale"})

	s.idx = &errIndex{index: s.idx, removeErr: errors.New("backend unavailable")}

	s.retentionOnce(ctx, 30) // must not panic or skip the prune step

	if len(ops.pruneCalls) != 1 {
		t.Fatalf("PruneMemoryOps calls = %d, want 1 (still attempted despite the remove error)", len(ops.pruneCalls))
	}
	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("remaining = %+v, want the point still present (remove failed)", remaining)
	}
}

// PruneMemoryOps failing must not undo the removal or panic; retentionOnce has no error return.
func TestRetentionOnce_PruneMemoryOpsErrorDoesNotAbort(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{pruneErr: errors.New("audit store unavailable")}
	s.SetOpsLog(ops)

	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	seedMemory(t, s, point{ID: "expired", Content: "long invalidated", Scope: "repo:r", Author: "a", Timestamp: "t",
		Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: old, InvalidationReason: "stale"})

	s.retentionOnce(ctx, 30) // must not panic despite PruneMemoryOps erroring

	remaining, _, err := s.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %+v, want the expired point removed regardless of the prune error", remaining)
	}
	if len(ops.pruneCalls) != 1 {
		t.Fatalf("PruneMemoryOps calls = %d, want 1", len(ops.pruneCalls))
	}
}

// The fingerprint changes with a member's content or the salt, not with member order.
func TestClusterFingerprint_ContentSensitiveOrderAndMemberInvariant(t *testing.T) {
	s := newSQLiteStore(t, "task", counting(new(int), `{"ops":[]}`))
	a := scored{ID: "m1", Content: "the frontend uses vite"}
	b := scored{ID: "m2", Content: "the backend is written in go"}

	base := s.clusterFingerprint([]scored{a, b})
	if got := s.clusterFingerprint([]scored{b, a}); got != base {
		t.Errorf("reordering members changed the fingerprint: %q vs %q", got, base)
	}

	edited := b
	edited.Content = "the backend is written in rust"
	if got := s.clusterFingerprint([]scored{a, edited}); got == base {
		t.Error("editing a member's content did not change the fingerprint")
	}

	otherDomain := newSQLiteStore(t, "user", counting(new(int), `{"ops":[]}`))
	if got := otherDomain.clusterFingerprint([]scored{a, b}); got == base {
		t.Error("a different domain (dedupe prompt) did not change the fingerprint")
	}

	otherModel := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[]}`})
	if got := otherModel.clusterFingerprint([]scored{a, b}); got == base {
		t.Error("a different consolidator model name did not change the fingerprint")
	}
}
