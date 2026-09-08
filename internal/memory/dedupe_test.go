package memory

import (
	"context"
	"testing"
)

// TestDedupeSweep_CrossChatClusterMerges covers issue #1269's core gap: two
// near-duplicate memories minted by DIFFERENT chats (so burstClusters never
// compares them) still get clustered and merged by DedupeSweep, with P5
// lineage carrying the absorbed point's votes into the survivor.
func TestDedupeSweep_CrossChatClusterMerges(t *testing.T) {
	ctx := context.Background()
	consolidator := fakeModel{reply: `{"ops":[{"action":"UPDATE","id":"a","content":"go/pkg/mod is a read-only symlink","kind":"convention"},` +
		`{"action":"DELETE","id":"b","reason":"duplicate of a"}]}`}
	s := newSQLiteStore(t, "task", consolidator)

	seedMemory(t, s, point{ID: "a", Content: "go/pkg/mod is a symlink to /usr/local/go/pkg/mod", Scope: "repo:x", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z", Vector: []float32{1, 0, 0, 0}})
	seedMemory(t, s, point{ID: "b", Content: "the go/pkg/mod dir is a root-owned read-only symlink", Scope: "repo:x", ChatID: "chat-2", MintedAt: "2026-09-03T00:00:00Z", Vector: []float32{0.99, 0.01, 0, 0}})
	seedMemory(t, s, point{ID: "c", Content: "CI runs 8 checks on every PR", Scope: "repo:x", ChatID: "chat-3", MintedAt: "2026-09-05T00:00:00Z", Vector: []float32{0, 1, 0, 0}})

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

	b, ok, err := s.idx.getByID(ctx, "b")
	if err != nil || !ok {
		t.Fatalf("getByID b: %v %v", ok, err)
	}
	if b.Status != string(StatusInvalidated) {
		t.Fatalf("b status = %q, want invalidated (absorbed)", b.Status)
	}

	a, ok, err := s.idx.getByID(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("getByID a: %v %v", ok, err)
	}
	if a.Status == string(StatusInvalidated) {
		t.Fatal("survivor a should stay live")
	}
}

// TestDedupeSweep_DryRunMakesNoLLMCall verifies dry run only clusters/reports -
// no consolidation call, nothing invalidated.
func TestDedupeSweep_DryRunMakesNoLLMCall(t *testing.T) {
	ctx := context.Background()
	calls := 0
	consolidator := countingErrModel{calls: &calls}
	s := newSQLiteStore(t, "task", consolidator)

	seedMemory(t, s, point{ID: "a", Content: "dup one", Scope: "repo:x", ChatID: "chat-1", MintedAt: "2026-08-13T00:00:00Z", Vector: []float32{1, 0, 0, 0}})
	seedMemory(t, s, point{ID: "b", Content: "dup two", Scope: "repo:x", ChatID: "chat-2", MintedAt: "2026-09-03T00:00:00Z", Vector: []float32{0.99, 0.01, 0, 0}})

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
}

// TestCosineClusters_BoundedSize proves the cluster-size cap: a chain of
// mutually-similar points longer than maxSize splits into more than one
// cluster rather than growing without bound.
func TestCosineClusters_BoundedSize(t *testing.T) {
	n := 6
	pts := make([]scored, n)
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		pts[i] = scored{ID: string(rune('a' + i))}
		vecs[i] = []float32{1, 0, 0, 0} // all mutually identical
	}
	clusters := cosineClusters(pts, vecs, 0.90, 3)
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

// TestMMRSelect_FiveNearDuplicatesYieldOne is the recall-diversity test
// (issue #1269 item 4): five near-identical points (mutual cosine >= 0.90)
// must not all occupy the top-k - at most one should survive mmrSelect.
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

// TestMMRSelect_DistinctHitsAllSurvive is the negative case: hits that are
// NOT near-duplicates of each other all pass through untouched.
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

// TestRecall_MMRDropsNearDuplicates is an end-to-end check that Store.recall
// itself (not just mmrSelect) applies the diversity re-rank: five
// near-identical points seeded with slightly different vectors, topK=5,
// should not all come back.
func TestRecall_MMRDropsNearDuplicates(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/mem.db"
	s, err := OpenSQLite(ctx, path, fakeEmbedder{}, nil, "test_dedupe_recall", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	for i := 0; i < 5; i++ {
		seedMemory(t, s, point{
			ID: string(rune('a' + i)), Content: "near duplicate fact", Scope: "repo:x",
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
}
