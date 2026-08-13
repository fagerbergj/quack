package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// upsertTimed writes one point with an explicit timestamp and a vector
// orthogonal to fakeEmbedder's fixed query vector ([1,0,0,0]) - so an
// embedding query can never rank it above minScore, but List (no embedding
// involved) still sees it. That contrast is exactly what TestListIsNotSearch checks.
func upsertTimed(t *testing.T, s *Store, id, scope, content, ts string) {
	t.Helper()
	if err := s.idx.upsert(context.Background(), []point{{
		ID: id, Vector: []float32{0, 1, 0, 0}, Content: content, Scope: scope, Author: "test", Timestamp: ts, Kind: "fact",
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

// Test case 1 (issue #727): listing is not search - it must surface entries an
// embedding query (with minScore set) would never rank in.
func TestListIsNotSearch(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil) // minScore=0.5 (see newSQLiteStore)

	upsertTimed(t, s, "a", "repo:x", "unrelated fact a", "2026-08-01T00:00:00Z")
	upsertTimed(t, s, "b", "repo:x", "unrelated fact b", "2026-08-02T00:00:00Z")
	upsertTimed(t, s, "c", "repo:x", "unrelated fact c", "2026-08-03T00:00:00Z")

	got, total, err := s.List(ctx, []string{"repo:x"}, 0, 10, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 || len(got) != 3 {
		t.Fatalf("List returned %d entries (total %d), want 3/3", len(got), total)
	}
	if got[0].ID != "c" || got[1].ID != "b" || got[2].ID != "a" {
		t.Fatalf("List order = [%s %s %s], want [c b a] (newest first)", got[0].ID, got[1].ID, got[2].ID)
	}

	// The same three points, via the query path a live recall would take:
	// orthogonal vectors score 0, below minScore, so nothing comes back. This
	// is the gap List closes - not a redundant path to the same answer.
	resp, err := s.recall(ctx, []string{"repo:x"}, "an unrelated query")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("recall (query) found %d of the 3 entries; want 0 - otherwise List proves nothing new", len(resp.Memories))
	}
}

// Test case 2: forgetting a memory has to reach the vector index, not just
// disappear from a listing view.
func TestForgetRemovesFromRecall(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	if err := s.idx.upsert(ctx, []point{{
		ID: "f1", Vector: []float32{1, 0, 0, 0}, Content: "NightsOut instrumentation tests need minSdk 30",
		Scope: "repo:x", Author: "test", Timestamp: "2026-08-01T00:00:00Z",
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	resp, err := s.recall(ctx, []string{"repo:x"}, "what minSdk do the instrumentation tests need")
	if err != nil {
		t.Fatalf("recall (before): %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall (before Forget) got %d, want 1", len(resp.Memories))
	}

	if err := s.Forget(ctx, "f1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	resp, err = s.recall(ctx, []string{"repo:x"}, "what minSdk do the instrumentation tests need")
	if err != nil {
		t.Fatalf("recall (after): %v", err)
	}
	if len(resp.Memories) != 0 {
		t.Fatalf("recall (after Forget) got %d, want 0 - deletion must reach the index", len(resp.Memories))
	}
}

func TestForgetUnknownIDReturnsNotFound(t *testing.T) {
	s := newSQLiteStore(t, "task", nil)
	if err := s.Forget(context.Background(), "does-not-exist"); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("Forget(unknown) = %v, want ErrMemoryNotFound", err)
	}
}

// Test case 3: the bucket filter is a real boundary, not a hint.
func TestListBucketFilterIsARealBoundary(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	upsertTimed(t, s, "a1", "repo:a", "fact about a", "2026-08-01T00:00:00Z")
	upsertTimed(t, s, "b1", "repo:b", "fact about b", "2026-08-01T00:00:01Z")

	got, total, err := s.List(ctx, []string{"repo:a"}, 0, 10, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("List(repo:a) = %+v (total %d), want exactly [a1]", got, total)
	}
}

// Test case 4: paging is stable across offsets - no duplicates, no omissions.
func TestListPagingIsStable(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("m%02d", i)
		ts := time.Date(2026, 8, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
		upsertTimed(t, s, id, "repo:x", "fact "+id, ts)
	}

	seen := map[string]bool{}
	var pageSizes []int
	for _, offset := range []int{0, 10, 20} {
		page, total, err := s.List(ctx, []string{"repo:x"}, offset, 10, false)
		if err != nil {
			t.Fatalf("List offset=%d: %v", offset, err)
		}
		if total != 25 {
			t.Fatalf("total at offset=%d = %d, want 25", offset, total)
		}
		pageSizes = append(pageSizes, len(page))
		for _, m := range page {
			if seen[m.ID] {
				t.Fatalf("id %s appeared on more than one page", m.ID)
			}
			seen[m.ID] = true
		}
	}
	if len(pageSizes) != 3 || pageSizes[0] != 10 || pageSizes[1] != 10 || pageSizes[2] != 5 {
		t.Fatalf("page sizes = %v, want [10 10 5]", pageSizes)
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d distinct ids across all pages, want 25 (no omissions)", len(seen))
	}
}

// Search (the ?q= path) carries a score and the bucket each hit came from,
// unlike List.
func TestSearchReturnsScoredEntries(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	if err := s.idx.upsert(ctx, []point{{
		ID: "s1", Vector: []float32{1, 0, 0, 0}, Content: "matches the query", Scope: "repo:x", Author: "test", Timestamp: "2026-08-01T00:00:00Z", Kind: "fact",
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.Search(ctx, []string{"repo:x"}, "anything", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s1" || got[0].Bucket != "repo:x" || got[0].Score == 0 {
		t.Fatalf("Search = %+v, want one scored hit in repo:x", got)
	}
}
