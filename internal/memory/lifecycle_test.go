package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpsLog records every memory_ops write for assertion, mirroring how a
// real internal/store-backed OpsLog would be called (see internal/serve's
// storeOpsLog adapter).
type fakeOpsLog struct {
	mu          sync.Mutex
	rows        []opRow
	pruneCalls  []time.Time
	pruneResult int
	pruneErr    error
}

type opRow struct {
	memoryID string
	op       OpsLogOp
	actor    OpsLogActor
	reason   string
}

func (f *fakeOpsLog) LogMemoryOp(_ context.Context, memoryID string, op OpsLogOp, actor OpsLogActor, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, opRow{memoryID, op, actor, reason})
	return nil
}

func (f *fakeOpsLog) PruneMemoryOps(_ context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneCalls = append(f.pruneCalls, cutoff)
	if f.pruneErr != nil {
		return 0, f.pruneErr
	}
	return f.pruneResult, nil
}

// TestApply_AddSetsLifecycleAndLogsOp covers design doc §4(a): a fresh ADD is
// unverified, reinforcement_count 0, valid_from stamped, and logs one
// memory_ops row.
func TestApply_AddSetsLifecycleAndLogsOp(t *testing.T) {
	const fixedNow = "2026-08-13T00:00:00Z"
	orig := nowRFC3339
	nowRFC3339 = func() string { return fixedNow }
	t.Cleanup(func() { nowRFC3339 = orig })

	ctx := context.Background()
	s := newSQLiteStore(t, "task", fakeModel{reply: `{"ops":[{"action":"ADD","content":"new fact","kind":"convention"}]}`})
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	if _, err := s.Commit(ctx, Scope{Repo: "r"}, "author", Provenance{}, nil, "some answer"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pts, err := s.idx.query(ctx, []string{"repo:r"}, []float32{1, 0, 0, 0}, 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d points, want 1", len(pts))
	}
	got := pts[0]
	if got.Status != string(StatusUnverified) {
		t.Fatalf("Status = %q, want %q", got.Status, StatusUnverified)
	}
	if got.ReinforcementCount != 0 {
		t.Fatalf("ReinforcementCount = %d, want 0", got.ReinforcementCount)
	}
	if got.ValidFrom != fixedNow {
		t.Fatalf("ValidFrom = %q, want %q", got.ValidFrom, fixedNow)
	}

	if len(ops.rows) != 1 {
		t.Fatalf("ops rows = %+v, want exactly 1", ops.rows)
	}
	if r := ops.rows[0]; r.memoryID != got.ID || r.op != OpAdd || r.actor != ActorConsolidator {
		t.Fatalf("op row = %+v, want {memoryID:%s op:add actor:consolidator}", r, got.ID)
	}
}

// TestApplyOutcome_Invalidate covers design doc §5/§7 case 1: an invalidated
// memory is soft-deleted (still present, queryable by id), excluded from
// recall at the backend-query level, and logs one memory_ops row.
func TestApplyOutcome_Invalidate(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	if err := s.idx.upsert(ctx, []point{{
		ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "bad advice from a poisoned run", Scope: "repo:r",
		Author: "a", Timestamp: "t", ChatID: "chat-x", Status: string(StatusUnverified), ValidFrom: "t",
	}}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	resp, err := s.recall(ctx, []string{"repo:r"}, "advice")
	if err != nil {
		t.Fatalf("recall (before): %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall (before) got %d, want 1", len(resp.Memories))
	}

	n, err := s.ApplyOutcome(ctx, []string{"m1"}, OutcomeSignal{Kind: OutcomeInvalidated, Reason: "pr closed unmerged"})
	if err != nil {
		t.Fatalf("ApplyOutcome: %v", err)
	}
	if n != 1 {
		t.Fatalf("ApplyOutcome touched %d, want 1", n)
	}

	resp, err = s.recall(ctx, []string{"repo:r"}, "advice")
	if err != nil {
		t.Fatalf("recall (after): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("recall (after invalidate) got %d, want 0 - excluded at the backend query", len(resp.Memories))
	}

	// Soft-delete only: the point still exists, reachable via list with
	// includeInvalidated=true (query never surfaces it regardless).
	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("list got %d, want 1 (invalidate must not remove the point)", len(pts))
	}
	if pts[0].Status != string(StatusInvalidated) || pts[0].InvalidationReason != "pr closed unmerged" {
		t.Fatalf("point after invalidate = %+v, want status=invalidated reason=%q", pts[0], "pr closed unmerged")
	}

	if len(ops.rows) != 1 {
		t.Fatalf("ops rows = %+v, want exactly 1", ops.rows)
	}
	if r := ops.rows[0]; r.memoryID != "m1" || r.op != OpInvalidate || r.actor != ActorOutcomeFeedback || r.reason != "pr closed unmerged" {
		t.Fatalf("op row = %+v, want {m1 invalidate outcome-feedback \"pr closed unmerged\"}", r)
	}
}

// TestApplyOutcome_Reinforce covers design doc §5/§7 case 2 plus the sticky
// invalidation rule: reinforce bumps unverified→reinforced and 0→1, a second
// reinforce bumps to ×2, and an already-invalidated memory is skipped even
// when its id is explicitly named (recalled-set semantics, epic #1255 P1).
func TestApplyOutcome_Reinforce(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	if err := s.idx.upsert(ctx, []point{
		{
			ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "a repo convention that held up", Scope: "repo:r",
			Author: "a", Timestamp: "t", ChatID: "chat-y", Status: string(StatusUnverified), ValidFrom: "t",
		},
		{
			// Already invalidated under the SAME chat_id - sticky rule: nothing
			// revives it, reinforce must skip it entirely.
			ID: "m2", Vector: []float32{1, 0, 0, 0}, Content: "already invalidated", Scope: "repo:r",
			Author: "a", Timestamp: "t", ChatID: "chat-y",
			Status: string(StatusInvalidated), ValidFrom: "t", InvalidatedAt: "t", InvalidationReason: "stale",
		},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	n, err := s.ApplyOutcome(ctx, []string{"m1", "m2"}, OutcomeSignal{Kind: OutcomeReinforced})
	if err != nil {
		t.Fatalf("ApplyOutcome: %v", err)
	}
	if n != 1 {
		t.Fatalf("ApplyOutcome touched %d, want 1 (sticky must skip the invalidated one)", n)
	}

	byID := func(t *testing.T) map[string]scored {
		t.Helper()
		pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := make(map[string]scored, len(pts))
		for _, p := range pts {
			out[p.ID] = p
		}
		return out
	}

	rows := byID(t)
	if rows["m1"].Status != string(StatusReinforced) || rows["m1"].ReinforcementCount != 1 {
		t.Fatalf("m1 after 1st reinforce = %+v, want status=reinforced count=1", rows["m1"])
	}
	if rows["m2"].Status != string(StatusInvalidated) || rows["m2"].ReinforcementCount != 0 {
		t.Fatalf("m2 (sticky) = %+v, want unchanged (still invalidated, count 0)", rows["m2"])
	}

	// A recalled reinforced memory carries the tier prefix with its count.
	resp, err := s.recall(ctx, []string{"repo:r"}, "convention")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall got %d, want 1 (m2 excluded, m1 included)", len(resp.Memories))
	}
	if text := extractText(resp.Memories[0]); !strings.HasPrefix(text, "[reinforced ×1] ") {
		t.Fatalf("recalled text = %q, want the reinforced ×1 tier prefix", text)
	}

	// Second reinforce bumps to ×2.
	if _, err := s.ApplyOutcome(ctx, []string{"m1", "m2"}, OutcomeSignal{Kind: OutcomeReinforced}); err != nil {
		t.Fatalf("ApplyOutcome (2nd): %v", err)
	}
	rows = byID(t)
	if rows["m1"].ReinforcementCount != 2 {
		t.Fatalf("m1 ReinforcementCount after 2nd reinforce = %d, want 2", rows["m1"].ReinforcementCount)
	}

	if len(ops.rows) != 2 {
		t.Fatalf("ops rows = %+v, want exactly 2 (one per reinforce, m2 never touched)", ops.rows)
	}
	for _, r := range ops.rows {
		if r.memoryID != "m1" || r.op != OpReinforce || r.actor != ActorOutcomeFeedback {
			t.Fatalf("op row = %+v, want {m1 reinforce outcome-feedback}", r)
		}
	}
}

// TestApplyVotes_SupportedAndContradicted covers epic #1255 P1: a supported
// vote is +1 upvote and flips tier to verified, a contradicted vote is +1
// downvote, and each vote writes one memory_ops row (actor=judge).
func TestApplyVotes_SupportedAndContradicted(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	if err := s.idx.upsert(ctx, []point{
		{ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "supported memory", Scope: "repo:r", Status: string(StatusUnverified)},
		{ID: "m2", Vector: []float32{1, 0, 0, 0}, Content: "contradicted memory", Scope: "repo:r", Status: string(StatusUnverified)},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	n, err := s.ApplyVotes(ctx, []Vote{
		{MemoryID: "m1", Vote: VoteSupported, Reason: "diff matches", Actor: ActorJudge},
		{MemoryID: "m2", Vote: VoteContradicted, Reason: "diff disagrees", Actor: ActorJudge},
	}, DefaultInvalidateThreshold)
	if err != nil {
		t.Fatalf("ApplyVotes: %v", err)
	}
	if n != 2 {
		t.Fatalf("ApplyVotes touched %d, want 2", n)
	}

	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]scored{}
	for _, p := range pts {
		byID[p.ID] = p
	}
	if m1 := byID["m1"]; m1.Upvotes != 1 || m1.Tier != TierVerified {
		t.Fatalf("m1 = %+v, want upvotes=1 tier=verified", m1)
	}
	if m2 := byID["m2"]; m2.Downvotes != 1 || m2.VoteScore != -1 || m2.Status == string(StatusInvalidated) {
		t.Fatalf("m2 = %+v, want downvotes=1 score=-1 not yet invalidated", m2)
	}

	if len(ops.rows) != 2 {
		t.Fatalf("ops rows = %+v, want exactly 2", ops.rows)
	}
	for _, r := range ops.rows {
		if r.op != OpVote || r.actor != ActorJudge {
			t.Fatalf("op row = %+v, want {op:vote actor:judge}", r)
		}
	}
}

// TestApplyVotes_NetScoreInvalidates covers the net-score invalidation rule:
// a second contradicted vote drops the score to the threshold and the
// memory is soft-invalidated with the fixed reason.
func TestApplyVotes_NetScoreInvalidates(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	if err := s.idx.upsert(ctx, []point{
		{ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "twice contradicted", Scope: "repo:r", Status: string(StatusUnverified)},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := s.ApplyVotes(ctx, []Vote{{MemoryID: "m1", Vote: VoteContradicted, Actor: ActorJudge}}, DefaultInvalidateThreshold); err != nil {
			t.Fatalf("ApplyVotes (%d): %v", i, err)
		}
	}

	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if pts[0].Status != string(StatusInvalidated) || pts[0].InvalidationReason != OutcomeReasonNetScore {
		t.Fatalf("m1 = %+v, want status=invalidated reason=%q", pts[0], OutcomeReasonNetScore)
	}
}

// TestApplyVotes_DuplicateVoteForSameMemoryCollapsesToOne covers the
// "duplicate votes for the same memory in one round" edge case: a judge
// that names the same id twice in one submit_verdict call must not double
// count - only the last vote applies.
func TestApplyVotes_DuplicateVoteForSameMemoryCollapsesToOne(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	if err := s.idx.upsert(ctx, []point{
		{ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "voted on twice in one round", Scope: "repo:r", Status: string(StatusUnverified)},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	// Same id twice: supported then contradicted - only the LAST should stick.
	n, err := s.ApplyVotes(ctx, []Vote{
		{MemoryID: "m1", Vote: VoteSupported, Actor: ActorJudge},
		{MemoryID: "m1", Vote: VoteContradicted, Actor: ActorJudge},
	}, DefaultInvalidateThreshold)
	if err != nil {
		t.Fatalf("ApplyVotes: %v", err)
	}
	if n != 1 {
		t.Fatalf("ApplyVotes touched %d, want 1 (deduped)", n)
	}

	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if pts[0].Upvotes != 0 || pts[0].Downvotes != 1 {
		t.Fatalf("m1 = %+v, want upvotes=0 downvotes=1 (last vote wins, no double count)", pts[0])
	}
}

// TestApplyVotes_SkipsAlreadyInvalidated covers stickiness: a vote for an
// already-invalidated memory is skipped entirely.
func TestApplyVotes_SkipsAlreadyInvalidated(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	if err := s.idx.upsert(ctx, []point{
		{ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "already gone", Scope: "repo:r", Status: string(StatusInvalidated), InvalidationReason: "stale"},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	n, err := s.ApplyVotes(ctx, []Vote{{MemoryID: "m1", Vote: VoteSupported, Actor: ActorJudge}}, DefaultInvalidateThreshold)
	if err != nil {
		t.Fatalf("ApplyVotes: %v", err)
	}
	if n != 0 {
		t.Fatalf("ApplyVotes touched %d, want 0 (sticky invalidation)", n)
	}
}

// TestApplyOutcome_SkipsVerifiedOnInvalidate covers design decision #1255:
// closed-unmerged only invalidates recalled-and-UNVERIFIED memories - a
// verified one recalled into the same chat gets no vote, not an invalidation.
func TestApplyOutcome_SkipsVerifiedOnInvalidate(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	if err := s.idx.upsert(ctx, []point{
		{ID: "unverified1", Vector: []float32{1, 0, 0, 0}, Content: "still unverified", Scope: "repo:r", Status: string(StatusUnverified)},
		{ID: "verified1", Vector: []float32{1, 0, 0, 0}, Content: "already verified", Scope: "repo:r", Status: string(StatusUnverified), Tier: TierVerified, Upvotes: 1},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	n, err := s.ApplyOutcome(ctx, []string{"unverified1", "verified1"}, OutcomeSignal{Kind: OutcomeInvalidated, Reason: OutcomeReasonClosedUnmerged})
	if err != nil {
		t.Fatalf("ApplyOutcome: %v", err)
	}
	if n != 1 {
		t.Fatalf("ApplyOutcome touched %d, want 1 (verified1 must be skipped)", n)
	}

	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]scored{}
	for _, p := range pts {
		byID[p.ID] = p
	}
	if byID["unverified1"].Status != string(StatusInvalidated) {
		t.Fatalf("unverified1 = %+v, want invalidated", byID["unverified1"])
	}
	if byID["verified1"].Status == string(StatusInvalidated) {
		t.Fatalf("verified1 = %+v, want untouched (verified memories are never demoted by closed-unmerged)", byID["verified1"])
	}
}

// TestBackfillTiers_IdempotentAcrossTwoBoots covers epic #1255 P1's
// migration: a point with reinforcement_count>=1 backfills to tier=verified
// with upvotes mirroring the count, a point with none backfills to
// unverified, and a second boot (a fresh OpenSQLite against the same file)
// touches neither again - a manually-set upvotes value from between boots
// survives untouched.
func TestBackfillTiers_IdempotentAcrossTwoBoots(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mem.db")

	s, err := OpenSQLite(ctx, path, fakeEmbedder{}, nil, "test_task", "task", 5, 0.5)
	if err != nil {
		t.Fatalf("OpenSQLite (boot 1): %v", err)
	}
	if err := s.idx.upsert(ctx, []point{
		{ID: "reinforced1", Vector: []float32{1, 0, 0, 0}, Content: "was reinforced pre-P1", Scope: "repo:r", Status: string(StatusReinforced), ReinforcementCount: 3},
		{ID: "fresh1", Vector: []float32{1, 0, 0, 0}, Content: "never reinforced", Scope: "repo:r", Status: string(StatusUnverified)},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	// upsert happened after boot 1's own (no-op, empty collection) backfill -
	// run it again directly, exactly as a boot that found existing data would.
	if _, err := s.idx.backfillTiers(ctx); err != nil {
		t.Fatalf("backfillTiers: %v", err)
	}

	byID := func() map[string]scored {
		pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := make(map[string]scored, len(pts))
		for _, p := range pts {
			out[p.ID] = p
		}
		return out
	}

	rows := byID()
	if r := rows["reinforced1"]; r.Tier != TierVerified || r.Upvotes != 3 {
		t.Fatalf("reinforced1 = %+v, want tier=verified upvotes=3", r)
	}
	if r := rows["fresh1"]; r.Tier != TierUnverified || r.Upvotes != 0 {
		t.Fatalf("fresh1 = %+v, want tier=unverified upvotes=0", r)
	}

	// Simulate a judge vote landing between boots - backfill must never touch
	// a point that already has a tier again.
	if _, err := s.ApplyVotes(ctx, []Vote{{MemoryID: "fresh1", Vote: VoteSupported, Actor: ActorJudge}}, DefaultInvalidateThreshold); err != nil {
		t.Fatalf("ApplyVotes: %v", err)
	}

	// Boot 2: a fresh OpenSQLite against the same file re-runs backfill.
	s2, err := OpenSQLite(ctx, path, fakeEmbedder{}, nil, "test_task", "task", 5, 0.5)
	if err != nil {
		t.Fatalf("OpenSQLite (boot 2): %v", err)
	}
	pts, err := s2.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list (boot 2): %v", err)
	}
	after := make(map[string]scored, len(pts))
	for _, p := range pts {
		after[p.ID] = p
	}
	if r := after["fresh1"]; r.Tier != TierVerified || r.Upvotes != 1 {
		t.Fatalf("fresh1 after boot 2 = %+v, want unchanged from its vote (tier=verified upvotes=1) - backfill must not re-touch it", r)
	}
	if r := after["reinforced1"]; r.Upvotes != 3 {
		t.Fatalf("reinforced1 after boot 2 = %+v, want still upvotes=3 (idempotent)", r)
	}
}

// TestInvalidateByID_HumanDelete covers design doc §7 case 5: a human delete
// via the REST handler invalidates by id (not chat_id, unlike ApplyOutcome),
// defaults the reason to "manual delete", writes one memory_ops row actor
// "human" op "invalidate", excludes the point from recall, and is idempotent
// on a second call against the same (now-invalidated) id.
func TestInvalidateByID_HumanDelete(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	if err := s.idx.upsert(ctx, []point{{
		ID: "m1", Vector: []float32{1, 0, 0, 0}, Content: "a memory a maintainer wants gone", Scope: "repo:r",
		Author: "a", Timestamp: "t", Status: string(StatusUnverified), ValidFrom: "t",
	}}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	if err := s.InvalidateByID(ctx, "m1", "", ActorHuman); err != nil {
		t.Fatalf("InvalidateByID: %v", err)
	}

	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pts) != 1 || pts[0].Status != string(StatusInvalidated) || pts[0].InvalidationReason != "manual delete" {
		t.Fatalf("point after delete = %+v, want status=invalidated reason=%q (default)", pts, "manual delete")
	}

	if len(ops.rows) != 1 {
		t.Fatalf("ops rows = %+v, want exactly 1", ops.rows)
	}
	if r := ops.rows[0]; r.memoryID != "m1" || r.op != OpInvalidate || r.actor != ActorHuman || r.reason != "manual delete" {
		t.Fatalf("op row = %+v, want {m1 invalidate human \"manual delete\"}", r)
	}

	resp, err := s.recall(ctx, []string{"repo:r"}, "a memory a maintainer wants gone")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("recall after delete got %d, want 0 - excluded at the backend query", len(resp.Memories))
	}

	// Deleting an already-invalidated id is idempotent: the point exists, so
	// it succeeds again rather than 404-ing, but nothing revives it.
	if err := s.InvalidateByID(ctx, "m1", "second delete", ActorHuman); err != nil {
		t.Fatalf("InvalidateByID (idempotent 2nd call): %v", err)
	}
	if len(ops.rows) != 2 {
		t.Fatalf("ops rows after 2nd delete = %+v, want exactly 2", ops.rows)
	}

	if err := s.InvalidateByID(ctx, "does-not-exist", "", ActorHuman); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("InvalidateByID(unknown) = %v, want ErrMemoryNotFound", err)
	}
}

// TestRecall_PreLifecyclePointsReadAsValidAndUnverified covers points minted
// by phase 1 (provenance-only, no status field at all): recall must still
// surface them (missing status ≠ invalidated) and tag them unverified.
func TestRecall_PreLifecyclePointsReadAsValidAndUnverified(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)

	if err := s.idx.upsert(ctx, []point{{
		ID: "legacy1", Vector: []float32{1, 0, 0, 0}, Content: "a phase-1 memory with no lifecycle fields",
		Scope: "repo:r", Author: "a", Timestamp: "t", ChatID: "old-chat", MintedAt: "t",
		// Status/ValidFrom/etc left zero-value - simulates a point written before this phase.
	}}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	resp, err := s.recall(ctx, []string{"repo:r"}, "phase-1 memory")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall got %d, want 1 (a missing status must read as valid)", len(resp.Memories))
	}
	if text := extractText(resp.Memories[0]); !strings.HasPrefix(text, "[unverified, single run] ") {
		t.Fatalf("recalled text = %q, want the unverified tier prefix", text)
	}
}

// TestApply_ConsolidatorDeleteInvalidatesWithReason covers design doc §4(a):
// the consolidator's DELETE soft-invalidates (never removes) and carries a
// reason, and the invalidated point drops out of the reconcile neighbour set
// so it can never be NOOP'd against or hallucination-targeted again.
func TestApply_ConsolidatorDeleteInvalidatesWithReason(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)

	if err := s.idx.upsert(ctx, []point{{
		ID: "dup1", Vector: []float32{1, 0, 0, 0}, Content: "duplicate fact", Scope: "repo:r",
		Author: "a", Timestamp: "t", Status: string(StatusUnverified), ValidFrom: "t",
	}}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	s.consolidator = fakeModel{reply: `{"ops":[{"action":"DELETE","id":"dup1","reason":"duplicate of new-id"}]}`}
	if _, err := s.Commit(ctx, Scope{Repo: "r"}, "author", Provenance{}, nil, "some answer"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pts, err := s.idx.list(ctx, []string{"repo:r"}, 0, 10, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("list got %d, want 1 (DELETE must invalidate in place, not remove)", len(pts))
	}
	if pts[0].Status != string(StatusInvalidated) || pts[0].InvalidationReason != "duplicate of new-id" {
		t.Fatalf("dup1 = %+v, want status=invalidated reason=%q", pts[0], "duplicate of new-id")
	}

	if len(ops.rows) != 1 {
		t.Fatalf("ops rows = %+v, want exactly 1", ops.rows)
	}
	if r := ops.rows[0]; r.memoryID != "dup1" || r.op != OpInvalidate || r.actor != ActorConsolidator || r.reason != "duplicate of new-id" {
		t.Fatalf("op row = %+v, want {dup1 invalidate consolidator \"duplicate of new-id\"}", r)
	}

	// Reconcile neighbour set excludes it: a legitimate re-add of the same
	// subject must not see dup1 and NOOP against it, nor can a later op name
	// its id (the "valid" map in apply() no longer contains it).
	neighbours, err := s.neighbours(ctx, "repo:r", "duplicate fact", nil)
	if err != nil {
		t.Fatalf("neighbours: %v", err)
	}
	if len(neighbours) != 0 {
		t.Fatalf("neighbours = %+v, want none (an invalidated point must be invisible to reconcile)", neighbours)
	}
}
