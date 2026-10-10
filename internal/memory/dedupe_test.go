package memory

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/adk/v2/model"
)

// Near-duplicates from different chats (never compared by burstClusters) still cluster and merge,
// with lineage carrying the absorbed point's votes into the survivor.
func TestDedupeSweep_CrossChatClusterMerges(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		aID, bID := testID("a"), testID("b")
		consolidator := fakeModel{reply: fmt.Sprintf(
			`{"ops":[{"action":"UPDATE","id":%q,"content":"go/pkg/mod is a read-only symlink","kind":"convention"},`+
				`{"action":"DELETE","id":%q,"reason":"duplicate of %s"}]}`, aID, bID, aID)}
		s := newStore("task", consolidator)

		seedMemory(t, s, point{ID: aID, Content: "go/pkg/mod is a symlink to /usr/local/go/pkg/mod", Scope: "repo:x", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z", Vector: []float32{1, 0, 0, 0}})
		seedMemory(t, s, point{ID: bID, Content: "the go/pkg/mod dir is a root-owned read-only symlink", Scope: "repo:x", ChatID: "chat-2", MintedAt: "2026-09-03T00:00:00Z", Vector: []float32{0.99, 0.01, 0, 0}})
		seedMemory(t, s, point{ID: testID("c"), Content: "CI runs 8 checks on every PR", Scope: "repo:x", ChatID: "chat-3", MintedAt: "2026-09-05T00:00:00Z", Vector: []float32{0, 1, 0, 0}})

		report, err := s.DedupeSweep(ctx, true)
		if err != nil {
			t.Fatalf("DedupeSweep: %v", err)
		}
		if report.NumClusters != 1 {
			t.Fatalf("clusters = %d, want 1 (a+b; c is unrelated)", report.NumClusters)
		}
		if report.LLMCalls != 1 {
			t.Fatalf("llm calls = %d, want 1", report.LLMCalls)
		}
		if report.OpsApplied == 0 {
			t.Fatal("expected ops applied (update survivor + invalidate absorbed)")
		}

		b, ok, err := s.idx.getByID(ctx, bID)
		if err != nil || !ok {
			t.Fatalf("getByID b: %v %v", ok, err)
		}
		if b.Status != string(StatusInvalidated) {
			t.Fatalf("b status = %q, want invalidated (absorbed)", b.Status)
		}

		a, ok, err := s.idx.getByID(ctx, aID)
		if err != nil || !ok {
			t.Fatalf("getByID a: %v %v", ok, err)
		}
		if a.Status == string(StatusInvalidated) {
			t.Fatal("survivor a should stay live")
		}
	})
}

// TestDedupeSweep_DryRunMakesNoLLMCall verifies dry run only clusters/reports -
// no consolidation call, nothing invalidated.
func TestDedupeSweep_DryRunMakesNoLLMCall(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		calls := 0
		consolidator := counting(&calls, "not valid json")
		s := newStore("task", consolidator)

		seedMemory(t, s, point{ID: testID("a"), Content: "dup one", Scope: "repo:x", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z", Vector: []float32{1, 0, 0, 0}})
		seedMemory(t, s, point{ID: testID("b"), Content: "dup two", Scope: "repo:x", ChatID: "chat-2", MintedAt: "2026-09-03T00:00:00Z", Vector: []float32{0.99, 0.01, 0, 0}})

		report, err := s.DedupeSweep(ctx, false)
		if err != nil {
			t.Fatalf("DedupeSweep: %v", err)
		}
		if report.NumClusters != 1 {
			t.Fatalf("clusters = %d, want 1", report.NumClusters)
		}
		if len(report.Clusters) != 1 || report.Clusters[0].Size != 2 {
			t.Fatalf("cluster report = %+v, want one cluster of 2", report.Clusters)
		}
		if calls != 0 {
			t.Fatalf("dry run made %d LLM calls, want 0", calls)
		}
		if report.LLMCalls != 0 || report.OpsApplied != 0 {
			t.Fatalf("dry run report = %+v, want zero calls/ops", report)
		}
	})
}

// A similar-point chain longer than maxSize splits into several clusters rather than growing unbounded.
func TestCosineClusters_BoundedSize(t *testing.T) {
	n := 6
	pts := make([]scored, n)
	for i := 0; i < n; i++ {
		pts[i] = scored{ID: string(rune('a' + i)), Vector: []float32{1, 0, 0, 0}} // all mutually identical
	}
	clusters := cosineClusters(pts, 0.90, 3)
	total := 0
	for _, c := range clusters {
		if len(c) > 3 {
			t.Fatalf("cluster size %d exceeds cap 3", len(c))
		}
		total += len(c)
	}
	if total != n {
		t.Fatalf("clustered %d of %d points", total, n)
	}
}

// Per-cluster order: highest VoteScore first (the survivor the prompt prefers), then oldest MintedAt.
func TestCosineClusters_SurvivorOrderPrefersHighestVotedThenOldest(t *testing.T) {
	pts := []scored{
		{ID: "newer-high-vote", VoteScore: 3, MintedAt: "2026-09-01T00:00:00Z", Vector: []float32{1, 0, 0, 0}},
		{ID: "older-low-vote", VoteScore: 0, MintedAt: "2026-01-01T00:00:00Z", Vector: []float32{1, 0, 0, 0}},
		{ID: "oldest-tied-vote", VoteScore: 0, MintedAt: "2025-01-01T00:00:00Z", Vector: []float32{1, 0, 0, 0}},
	}
	clusters := cosineClusters(pts, 0.90, 10)
	if len(clusters) != 1 || len(clusters[0]) != 3 {
		t.Fatalf("clusters = %+v, want one cluster of 3", clusters)
	}
	got := clusters[0]
	if got[0].ID != "newer-high-vote" {
		t.Fatalf("cluster[0] = %q, want the highest-voted member first", got[0].ID)
	}
	if got[1].ID != "oldest-tied-vote" {
		t.Fatalf("cluster[1] = %q, want the older of the two zero-vote members next", got[1].ID)
	}
}

// DedupeSweep clusters on stored vectors and never re-embeds. Sqlite-only: forEachBackend can't take a
// custom embedder, and the code path is backend-independent.
func TestDedupeSweep_NoEmbedderCalls(t *testing.T) {
	ctx := context.Background()
	ce := &countingEmbedder{}
	s, err := OpenSQLite(ctx, t.TempDir()+"/mem.db", ce, fakeModel{reply: `{"ops":[]}`}, "test_task", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if ce.calls != 0 {
		t.Fatalf("OpenSQLite itself embedded %d times (sqlite's ensure() has no fixed-dimension probe)", ce.calls)
	}

	seedMemory(t, s, point{ID: "a", Content: "dup one", Scope: "repo:x", MintedAt: "2026-08-13T00:00:00Z", Vector: []float32{1, 0, 0, 0}})
	seedMemory(t, s, point{ID: "b", Content: "dup two", Scope: "repo:x", MintedAt: "2026-09-03T00:00:00Z", Vector: []float32{0.99, 0.01, 0, 0}})

	if _, err := s.DedupeSweep(ctx, true); err != nil {
		t.Fatalf("DedupeSweep: %v", err)
	}
	if ce.calls != 0 {
		t.Fatalf("DedupeSweep made %d embedder calls, want 0 (cluster on the stored vector, never re-embed)", ce.calls)
	}
}

// A verified pair is still eligible for clustering, and its votes sum onto the survivor.
func TestDedupeSweep_VerifiedPairMergesWithSummedVotes(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		survivorID, dupID := testID("survivor"), testID("dup")
		consolidator := fakeModel{reply: fmt.Sprintf(
			`{"ops":[{"action":"UPDATE","id":%q,"content":"merged wording","kind":"convention"},`+
				`{"action":"DELETE","id":%q,"reason":"duplicate of %s"}]}`, survivorID, dupID, survivorID)}
		s := newStore("task", consolidator)

		seedMemory(t, s, point{ID: survivorID, Content: "run make test, not go test", Scope: "repo:x",
			Status: string(StatusReinforced), Tier: TierVerified, Upvotes: 2, VoteScore: 2, Vector: []float32{1, 0, 0, 0}})
		seedMemory(t, s, point{ID: dupID, Content: "use make test, never go test", Scope: "repo:x",
			Status: string(StatusReinforced), Tier: TierVerified, Upvotes: 1, VoteScore: 1, Vector: []float32{0.99, 0.01, 0, 0}})

		report, err := s.DedupeSweep(ctx, true)
		if err != nil {
			t.Fatalf("DedupeSweep: %v", err)
		}
		if report.NumClusters != 1 {
			t.Fatalf("clusters = %d, want 1 (verified points must not be excluded from clustering)", report.NumClusters)
		}

		sv, ok, err := s.idx.getByID(ctx, survivorID)
		if err != nil || !ok {
			t.Fatalf("getByID survivor: %v %v", ok, err)
		}
		if sv.VoteScore != 3 || sv.Upvotes != 3 {
			t.Fatalf("survivor votes = +%d score %d, want +3 score 3 (summed from both verified points)", sv.Upvotes, sv.VoteScore)
		}
		if sv.Status == string(StatusInvalidated) {
			t.Fatal("survivor should stay live")
		}

		dup, ok, err := s.idx.getByID(ctx, dupID)
		if err != nil || !ok {
			t.Fatalf("getByID dup: %v %v", ok, err)
		}
		if dup.Status != string(StatusInvalidated) {
			t.Fatalf("dup status = %q, want invalidated (absorbed)", dup.Status)
		}
	})
}

// Five near-identical points (mutual cosine >= 0.90) collapse to at most one in mmrSelect's top-k.
func TestMMRSelect_FiveNearDuplicatesYieldOne(t *testing.T) {
	pts := make([]scored, 5)
	for i := range pts {
		pts[i] = scored{ID: string(rune('a' + i)), Score: float32(5 - i), Vector: []float32{1, 0.001 * float32(i), 0, 0}}
	}
	got := mmrSelect(pts, 5, 0.90)
	if len(got) != 1 {
		t.Fatalf("mmrSelect kept %d of 5 near-duplicates, want 1", len(got))
	}
	if got[0].ID != "a" {
		t.Fatalf("mmrSelect kept %q, want the highest-scoring \"a\"", got[0].ID)
	}
}

// Hits that are not near-duplicates all pass through untouched.
func TestMMRSelect_DistinctHitsAllSurvive(t *testing.T) {
	pts := []scored{
		{ID: "a", Score: 3, Vector: []float32{1, 0, 0, 0}},
		{ID: "b", Score: 2, Vector: []float32{0, 1, 0, 0}},
		{ID: "c", Score: 1, Vector: []float32{0, 0, 1, 0}},
	}
	got := mmrSelect(pts, 3, 0.90)
	if len(got) != 3 {
		t.Fatalf("mmrSelect kept %d of 3 distinct hits, want 3", len(got))
	}
}

// Store.recall itself applies the MMR re-rank, through each backend's query-with-vectors plumbing.
func TestRecall_MMRDropsNearDuplicates(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		for i := 0; i < 5; i++ {
			seedMemory(t, s, point{
				ID: testID(fmt.Sprintf("dup-%d", i)), Content: "near duplicate fact", Scope: "repo:x",
				Vector: []float32{1, 0.001 * float32(i), 0, 0}, // fakeEmbedder always queries [1,0,0,0]
			})
		}
		_, hits, err := s.recall(ctx, []string{"repo:x"}, "anything")
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("recall returned %d of 5 near-identical hits, want at most 1", len(hits))
		}
	})
}
