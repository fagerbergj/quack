package vetting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
)

// failingLedger wraps a real LedgerStore and fails AppendIntent for one entry kind, to test
// memory.vote's fail-closed path against a genuine append failure.
type failingLedger struct {
	*ledgertest.MemStore
	failKind string
}

func (f *failingLedger) AppendIntent(ctx context.Context, e ledger.Entry) (int64, error) {
	if e.Kind == f.failKind {
		return 0, errors.New("simulated ledger append failure")
	}
	return f.MemStore.AppendIntent(ctx, e)
}

func newMemoryStoreForVoteTest(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.OpenSQLite(context.Background(), t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_votes", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

// One supported and one contradicted memory yield +1/-1 and a memory.vote entry each, and the
// supported one's tier flips to verified.
func TestApplyMemoryVotesOnPass_SupportedAndContradicted(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	// Seed two memories directly so their ids are known.
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "supported fact"}}, ""); err != nil {
		t.Fatalf("commit m1: %v", err)
	}
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "contradicted fact"}}, ""); err != nil {
		t.Fatalf("commit m2: %v", err)
	}
	mems, _, err := store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if err != nil || len(mems) != 2 {
		t.Fatalf("List: %v mems=%+v", err, mems)
	}
	var m1, m2 string
	for _, m := range mems {
		switch m.Content {
		case "supported fact":
			m1 = m.ID
		case "contradicted fact":
			m2 = m.ID
		}
	}
	if m1 == "" || m2 == "" {
		t.Fatalf("did not find both seeded memories: %+v", mems)
	}

	lgr := ledgertest.NewMemStore()
	cfg := Config{ChatID: "chat1", Memory: store, Ledger: lgr}
	received := []memory.Delivered{{ID: m1, Content: "supported fact"}, {ID: m2, Content: "contradicted fact"}}
	votes := []memoryVerdict{
		{ID: m1, Vote: memory.VoteSupported, Reason: "diff matches"},
		{ID: m2, Vote: memory.VoteContradicted, Reason: "diff disagrees"},
	}

	applyMemoryVotesOnPass(ctx, cfg, "node1", 1, received, votes)

	mems, _, err = store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if err != nil {
		t.Fatalf("List after votes: %v", err)
	}
	byID := map[string]memory.Memory{}
	for _, m := range mems {
		byID[m.ID] = m
	}
	if g := byID[m1]; g.Upvotes != 1 || g.Tier != memory.TierVerified {
		t.Fatalf("m1 = %+v, want upvotes=1 tier=verified", g)
	}
	if g := byID[m2]; g.Downvotes != 1 {
		t.Fatalf("m2 = %+v, want downvotes=1", g)
	}

	entries, err := lgr.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	var voteEntries int
	for _, e := range entries {
		if e.Kind == ledger.KindMemoryVote {
			voteEntries++
		}
	}
	if voteEntries != 2 {
		t.Fatalf("memory.vote ledger entries = %d, want 2", voteEntries)
	}
}

// A vote for an id the worker was never given is dropped, never trusted.
func TestApplyMemoryVotesOnPass_IgnoresVoteForUnknownID(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "only memory"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mems, _, _ := store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	m1 := mems[0].ID

	cfg := Config{ChatID: "chat1", Memory: store, Ledger: ledgertest.NewMemStore()}
	received := []memory.Delivered{{ID: m1, Content: "only memory"}}
	votes := []memoryVerdict{{ID: "not-a-real-id", Vote: memory.VoteSupported}}

	applyMemoryVotesOnPass(ctx, cfg, "node1", 1, received, votes)

	mems, _, _ = store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if mems[0].Upvotes != 0 {
		t.Fatalf("m1 upvotes = %d, want 0 (vote for an unreceived id must be dropped)", mems[0].Upvotes)
	}
}

// A recall delivery appends one memory.recall entry naming every delivered id, stamped with the
// node's agent and round.
func TestRecallLedgerEntry_AppendsMemoryRecall(t *testing.T) {
	ctx := context.Background()
	lgr := ledgertest.NewMemStore()
	cfg := Config{ChatID: "chat1", Agent: "worker", Ledger: lgr}
	hits := []memory.Delivered{{ID: "m1"}, {ID: "m2"}}

	recallLedgerEntry(ctx, cfg, "node1", 2, "prefill", hits)

	entries, err := lgr.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != ledger.KindMemoryRecall {
		t.Fatalf("entries = %+v, want exactly one memory.recall", entries)
	}
	if entries[0].Agent != "worker" || entries[0].Round != "2" {
		t.Fatalf("entry coords = agent=%q round=%q, want agent=worker round=2", entries[0].Agent, entries[0].Round)
	}
}

// The point mutation projects the memory.vote entry, so a failed AppendIntent skips the mutation AND
// the memory_ops row: never apply a vote the ledger didn't record.
func TestApplyMemoryVotesOnPass_LedgerAppendFailureSkipsMutation(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "vote that never durably lands"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mems, _, _ := store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	m1 := mems[0].ID

	ops := &fakeOpsLogRecorder{}
	store.SetOpsLog(ops)

	lgr := &failingLedger{MemStore: ledgertest.NewMemStore(), failKind: ledger.KindMemoryVote}
	cfg := Config{ChatID: "chat1", Memory: store, Ledger: lgr}
	received := []memory.Delivered{{ID: m1, Content: "vote that never durably lands"}}
	votes := []memoryVerdict{{ID: m1, Vote: memory.VoteSupported, Reason: "would have applied"}}

	applyMemoryVotesOnPass(ctx, cfg, "node1", 1, received, votes)

	mems, _, _ = store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if mems[0].Upvotes != 0 || mems[0].Tier == memory.TierVerified {
		t.Fatalf("m1 = %+v, want untouched (ledger append failed, so nothing may be projected)", mems[0])
	}
	if len(ops.rows) != 0 {
		t.Fatalf("memory_ops rows = %+v, want none (no vote row for an entry that never landed)", ops.rows)
	}
	entries, err := lgr.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ledger entries = %+v, want none (the failing append must not have landed anything)", entries)
	}
}

// A memory recalled by both prefill and recall_memory appears once in the received set (no double vote).
func TestMergeMemoryHits_DedupesByID(t *testing.T) {
	base := []memory.Delivered{{ID: "m1", Content: "from prefill"}}
	add := []memory.Delivered{{ID: "m1", Content: "from tool call"}, {ID: "m2", Content: "new"}}
	got, added := mergeMemoryHits(base, add)
	if len(got) != 2 {
		t.Fatalf("merged = %+v, want 2 (m1 deduped, m2 added)", got)
	}
	if got[0].ID != "m1" || got[0].Content != "from prefill" {
		t.Fatalf("m1 = %+v, want the FIRST occurrence kept (prefill's), not overwritten by the tool's", got[0])
	}
	if len(added) != 1 || added[0].ID != "m2" {
		t.Fatalf("added = %+v, want only m2 (m1 already in base)", added)
	}
	// Merging again with the same add must not grow the set further, and nothing new is added.
	got2, added2 := mergeMemoryHits(got, add)
	if len(got2) != 2 {
		t.Fatalf("re-merge grew the set: %+v", got2)
	}
	if len(added2) != 0 {
		t.Fatalf("re-merge added = %+v, want none (both ids already received)", added2)
	}
}

// A native worker's recall_memory FunctionResponse, round-tripped through session-event JSON, parses
// back into the hits it returned.
func TestRecallMemoryHits_ParsesFunctionResponse(t *testing.T) {
	resp := map[string]any{
		"hits": []any{
			map[string]any{"id": "m1", "tier": "unverified", "score": 0.9, "content": "a fact"},
		},
		"truncated": false,
	}
	hits := recallMemoryHits(resp)
	if len(hits) != 1 || hits[0].ID != "m1" || hits[0].Content != "a fact" {
		t.Fatalf("recallMemoryHits = %+v, want one hit for m1", hits)
	}
	if got := recallMemoryHits(nil); got != nil {
		t.Fatalf("nil response: got %+v, want nil", got)
	}
}

// TestActivityScanner_LoadMemoryReachesReceivedSet: load_memory must scan into
// act.recalled like recall_memory - prepareJudge merges that into the received set.
func TestActivityScanner_LoadMemoryReachesReceivedSet(t *testing.T) {
	resp := map[string]any{"hits": []any{
		map[string]any{"id": "m1", "tier": "unverified", "score": 0.9, "content": "a fact"},
	}}
	act := activityFromSessionAt(newTestSession(t, fnResp("1", "load_memory", resp)), "", "")
	if len(act.recalled) != 1 || act.recalled[0].ID != "m1" || act.recalled[0].Score != 0.9 {
		t.Fatalf("recalled = %+v, want one hit for m1 with its score", act.recalled)
	}
}

// fakeOpsLogRecorder records every memory_ops write, mirroring
// internal/memory's own lifecycle_test.go fixture (unexported there).
type fakeOpsLogRecorder struct {
	rows []struct {
		memoryID string
		op       memory.OpsLogOp
		actor    memory.OpsLogActor
		reason   string
	}
}

func (f *fakeOpsLogRecorder) LogMemoryOp(_ context.Context, memoryID string, op memory.OpsLogOp, actor memory.OpsLogActor, reason string) error {
	f.rows = append(f.rows, struct {
		memoryID string
		op       memory.OpsLogOp
		actor    memory.OpsLogActor
		reason   string
	}{memoryID, op, actor, reason})
	return nil
}

func (f *fakeOpsLogRecorder) PruneMemoryOps(context.Context, time.Time) (int, error) { return 0, nil }

// An empty verdict.Memories applies no votes (the shape a failed round would carry).
func TestApplyMemoryVotesOnPass_NoVotesWhenNoneGiven(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "untouched"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mems, _, _ := store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	m1 := mems[0].ID

	lgr := ledgertest.NewMemStore()
	cfg := Config{ChatID: "chat1", Memory: store, Ledger: lgr}
	applyMemoryVotesOnPass(ctx, cfg, "node1", 1, []memory.Delivered{{ID: m1}}, nil)

	mems, _, _ = store.List(ctx, []string{"repo:r"}, 0, 10, true, "")
	if mems[0].Upvotes != 0 || mems[0].Downvotes != 0 {
		t.Fatalf("m1 = %+v, want untouched", mems[0])
	}
	entries, _ := lgr.ReadEntries(ctx, "chat1", 0)
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
}

// A verdict voting on SOME received memories is still missing votes, since only ids present in
// v.Memories are applied.
func TestMissingMemoryVotes_PartialVoteStillMissing(t *testing.T) {
	received := []string{"m1", "m2", "m3"}
	cases := []struct {
		name string
		v    verdict
		want bool
	}{
		{"none voted", verdict{}, true},
		{"partial vote", verdict{Memories: []memoryVerdict{{ID: "m1", Vote: "supported"}}}, true},
		{"all voted", verdict{Memories: []memoryVerdict{
			{ID: "m1", Vote: "supported"}, {ID: "m2", Vote: "not_relevant"}, {ID: "m3", Vote: "contradicted"},
		}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := missingMemoryVotes(received, c.v); got != c.want {
				t.Errorf("missingMemoryVotes(%v, %+v) = %v, want %v", received, c.v, got, c.want)
			}
		})
	}
	if missingMemoryVotes(nil, verdict{}) {
		t.Error("missingMemoryVotes(nil, ...) = true, want false (nothing was owed)")
	}
}

// A vote on an id never received must not cover a different, unvoted received id: the check keys off
// receivedIDs, not v.Memories.
func TestMissingMemoryVotes_VoteOutsideRecallSetDoesNotCountAsCoverage(t *testing.T) {
	v := verdict{Memories: []memoryVerdict{{ID: "m2", Vote: "supported"}}}
	if !missingMemoryVotes([]string{"m1"}, v) {
		t.Error("missingMemoryVotes([m1], votes=[m2]) = false, want true (m1 was never voted on)")
	}
}
