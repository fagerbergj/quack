package vetting

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
)

func newMemoryStoreForVoteTest(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.OpenSQLite(context.Background(), t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_votes", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

// TestApplyMemoryVotesOnPass_SupportedAndContradicted covers epic #1255 P1's
// core verification: two recalled memories, one supported and one
// contradicted, yield +1/-1 and a memory.vote ledger entry each, and the
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
	mems, _, err := store.List(ctx, []string{"repo:r"}, 0, 10, true)
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

	mems, _, err = store.List(ctx, []string{"repo:r"}, 0, 10, true)
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

// TestApplyMemoryVotesOnPass_IgnoresVoteForUnknownID covers the "the judge
// named a memory it was never given" hardening: a vote whose id isn't in the
// received set is dropped, never trusted blindly.
func TestApplyMemoryVotesOnPass_IgnoresVoteForUnknownID(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "only memory"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mems, _, _ := store.List(ctx, []string{"repo:r"}, 0, 10, true)
	m1 := mems[0].ID

	cfg := Config{ChatID: "chat1", Memory: store, Ledger: ledgertest.NewMemStore()}
	received := []memory.Delivered{{ID: m1, Content: "only memory"}}
	votes := []memoryVerdict{{ID: "not-a-real-id", Vote: memory.VoteSupported}}

	applyMemoryVotesOnPass(ctx, cfg, "node1", 1, received, votes)

	mems, _, _ = store.List(ctx, []string{"repo:r"}, 0, 10, true)
	if mems[0].Upvotes != 0 {
		t.Fatalf("m1 upvotes = %d, want 0 (vote for an unreceived id must be dropped)", mems[0].Upvotes)
	}
}

// TestRecallLedgerEntry_AppendsMemoryRecall covers the usage-tracking half:
// a recall delivery appends one memory.recall ledger entry naming every
// delivered id.
func TestRecallLedgerEntry_AppendsMemoryRecall(t *testing.T) {
	ctx := context.Background()
	lgr := ledgertest.NewMemStore()
	cfg := Config{ChatID: "chat1", Ledger: lgr}
	hits := []memory.Delivered{{ID: "m1"}, {ID: "m2"}}

	recallLedgerEntry(ctx, cfg, "node1", 0, "prefill", hits)

	entries, err := lgr.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != ledger.KindMemoryRecall {
		t.Fatalf("entries = %+v, want exactly one memory.recall", entries)
	}
}

// TestApplyMemoryVotesOnPass_NoVotesWhenNoneGiven covers "a failed round
// yields no votes": RunGatedRefine never calls applyMemoryVotesOnPass for a
// failed round (see node.go's res.Passed guard), but this pins the
// function's own behavior when called with an empty verdict.Memories, the
// shape a failed round's zero-value verdict would carry if it were.
func TestApplyMemoryVotesOnPass_NoVotesWhenNoneGiven(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStoreForVoteTest(t)
	if _, err := store.Commit(ctx, memory.Scope{Repo: "r"}, "author", memory.Provenance{ChatID: "chat1"},
		[]memory.Candidate{{Content: "untouched"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mems, _, _ := store.List(ctx, []string{"repo:r"}, 0, 10, true)
	m1 := mems[0].ID

	lgr := ledgertest.NewMemStore()
	cfg := Config{ChatID: "chat1", Memory: store, Ledger: lgr}
	applyMemoryVotesOnPass(ctx, cfg, "node1", 1, []memory.Delivered{{ID: m1}}, nil)

	mems, _, _ = store.List(ctx, []string{"repo:r"}, 0, 10, true)
	if mems[0].Upvotes != 0 || mems[0].Downvotes != 0 {
		t.Fatalf("m1 = %+v, want untouched", mems[0])
	}
	entries, _ := lgr.ReadEntries(ctx, "chat1", 0)
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
}
