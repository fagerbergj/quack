package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
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
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil) // minScore=0.5 (see newSQLiteStore/newQdrantStore)
		aID, bID, cID := testID("a"), testID("b"), testID("c")

		upsertTimed(t, s, aID, "repo:x", "unrelated fact a", "2026-08-01T00:00:00Z")
		upsertTimed(t, s, bID, "repo:x", "unrelated fact b", "2026-08-02T00:00:00Z")
		upsertTimed(t, s, cID, "repo:x", "unrelated fact c", "2026-08-03T00:00:00Z")

		got, total, err := s.List(ctx, []string{"repo:x"}, 0, 10, false, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 || len(got) != 3 {
			t.Fatalf("List returned %d entries (total %d), want 3/3", len(got), total)
		}
		if got[0].ID != cID || got[1].ID != bID || got[2].ID != aID {
			t.Fatalf("List order = [%s %s %s], want [c b a] (newest first)", got[0].ID, got[1].ID, got[2].ID)
		}

		// The same three points, via the query path a live recall would take:
		// orthogonal vectors score 0, below minScore, so nothing comes back. This
		// is the gap List closes - not a redundant path to the same answer.
		resp, _, err := s.recall(ctx, []string{"repo:x"}, "an unrelated query")
		if err != nil {
			t.Fatalf("recall: %v", err)
		}
		if len(resp.Memories) != 0 {
			t.Fatalf("recall (query) found %d of the 3 entries; want 0 - otherwise List proves nothing new", len(resp.Memories))
		}
	})
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

	resp, _, err := s.recall(ctx, []string{"repo:x"}, "what minSdk do the instrumentation tests need")
	if err != nil {
		t.Fatalf("recall (before): %v", err)
	}
	if len(resp.Memories) != 1 {
		t.Fatalf("recall (before Forget) got %d, want 1", len(resp.Memories))
	}

	if err := s.Forget(ctx, "f1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	resp, _, err = s.recall(ctx, []string{"repo:x"}, "what minSdk do the instrumentation tests need")
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

	got, total, err := s.List(ctx, []string{"repo:a"}, 0, 10, false, "")
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
		page, total, err := s.List(ctx, []string{"repo:x"}, offset, 10, false, "")
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

// Test case 5 (issue #878 review): paging stays stable once invalidated
// entries are in the mix - no duplicates, no omissions, across pages.
func TestListPagingIncludeInvalidated_Mixed(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ids := []string{"m00", "m01", "m02", "m03", "m04"}
	for i, id := range ids {
		ts := time.Date(2026, 8, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
		upsertTimed(t, s, id, "repo:x", "fact "+id, ts)
	}
	for _, id := range []string{"m01", "m03"} {
		if err := s.InvalidateByID(ctx, id, "test", ActorOutcomeFeedback); err != nil {
			t.Fatalf("InvalidateByID(%s): %v", id, err)
		}
	}

	seen := map[string]bool{}
	for _, offset := range []int{0, 2, 4} {
		page, total, err := s.List(ctx, []string{"repo:x"}, offset, 2, true, "")
		if err != nil {
			t.Fatalf("List offset=%d: %v", offset, err)
		}
		if total != 5 {
			t.Fatalf("total at offset=%d = %d, want 5", offset, total)
		}
		for _, m := range page {
			if seen[m.ID] {
				t.Fatalf("id %s appeared on more than one page", m.ID)
			}
			seen[m.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d distinct ids across all pages, want 5 (no omissions)", len(seen))
	}
}

// TestList_TierFilterSpansPages is #1265 review finding 10: the tier filter
// is index-level (a WHERE clause / Qdrant condition), not a client-side
// post-filter over one page - it must apply across a paged List correctly,
// with total/paging agreeing with the filter. Also covers a legacy point
// with no tier at all reading as "unverified" under the filter (design doc
// §3/toMemories' wire mapping rule extended to this filter).
func TestList_TierFilterSpansPages(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		v1ID, v2ID, u1ID, u2ID := testID("v1"), testID("v2"), testID("u1"), testID("u2")
		if err := s.idx.upsert(ctx, []point{
			{ID: v1ID, Vector: []float32{0, 1, 0, 0}, Content: "verified 1", Scope: "repo:x", Timestamp: "2026-08-01T00:00:00Z", Tier: TierVerified},
			{ID: v2ID, Vector: []float32{0, 1, 0, 0}, Content: "verified 2", Scope: "repo:x", Timestamp: "2026-08-02T00:00:00Z", Tier: TierVerified},
			{ID: u1ID, Vector: []float32{0, 1, 0, 0}, Content: "unverified explicit", Scope: "repo:x", Timestamp: "2026-08-03T00:00:00Z", Tier: TierUnverified},
			{ID: u2ID, Vector: []float32{0, 1, 0, 0}, Content: "unverified legacy (no tier field)", Scope: "repo:x", Timestamp: "2026-08-04T00:00:00Z"},
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		verified, total, err := s.List(ctx, []string{"repo:x"}, 0, 1, false, TierVerified)
		if err != nil {
			t.Fatalf("List verified: %v", err)
		}
		if total != 2 {
			t.Fatalf("verified total = %d, want 2 (must span pages, not just page 0)", total)
		}
		if len(verified) != 1 {
			t.Fatalf("verified page = %+v, want exactly 1 (limit=1)", verified)
		}

		unverified, total, err := s.List(ctx, []string{"repo:x"}, 0, 10, false, TierUnverified)
		if err != nil {
			t.Fatalf("List unverified: %v", err)
		}
		if total != 2 {
			t.Fatalf("unverified total = %d, want 2 (u1 explicit + u2 legacy no-tier)", total)
		}
		gotIDs := map[string]bool{}
		for _, m := range unverified {
			gotIDs[m.ID] = true
		}
		if !gotIDs[u1ID] || !gotIDs[u2ID] {
			t.Fatalf("unverified = %+v, want u1 and u2 (a legacy no-tier point reads as unverified)", unverified)
		}
	})
}

// TestGetByID_FindsAcrossBackendsAndTiersInvalidated covers #1268: GetByID is
// a direct id lookup (not a page scan), and it returns an invalidated point
// too - the caller decides what an invalidated Status means, unlike List's
// default exclusion.
func TestGetByID_FindsAcrossBackendsAndTiersInvalidated(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		v1ID, goneID := testID("v1"), testID("gone")
		if err := s.idx.upsert(ctx, []point{
			{ID: v1ID, Vector: []float32{0, 1, 0, 0}, Content: "verified", Scope: "repo:x", Tier: TierVerified},
			{ID: goneID, Vector: []float32{0, 1, 0, 0}, Content: "invalidated", Scope: "repo:x", Status: string(StatusInvalidated), InvalidationReason: "stale"},
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		m, err := s.GetByID(ctx, v1ID)
		if err != nil {
			t.Fatalf("GetByID v1: %v", err)
		}
		if m.Tier != TierVerified {
			t.Fatalf("v1 tier = %q, want verified", m.Tier)
		}

		inv, err := s.GetByID(ctx, goneID)
		if err != nil {
			t.Fatalf("GetByID gone: %v", err)
		}
		if inv.Status != string(StatusInvalidated) || inv.InvalidationReason != "stale" {
			t.Fatalf("gone = %+v, want status=invalidated reason=stale (GetByID returns invalidated points too)", inv)
		}

		if _, err := s.GetByID(ctx, testID("does-not-exist")); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("GetByID(unknown) = %v, want ErrMemoryNotFound", err)
		}

		// #1268 bug: a malformed (non-UUID) id used to reach qdrant's client
		// unvalidated and come back as a raw "Unable to parse UUID" gRPC error
		// instead of ErrMemoryNotFound - breaking findMemoryByID's try-each-store
		// fallback in internal/server/rest/memory.go. sqlite never had this
		// failure mode (a WHERE-clause miss on any string), so only the qdrant
		// subtest actually exercised it before the fix in qdrant.go's
		// idsToPointIDs.
		if _, err := s.GetByID(ctx, "not-a-uuid"); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("GetByID(malformed id) = %v, want ErrMemoryNotFound (not a raw backend error)", err)
		}
	})
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
