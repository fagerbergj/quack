package memory

import (
	"sort"
	"testing"
)

// TestQdrantLess (#1266 review): qdrant's list() sorts entirely in Go (it
// already fetches the whole matching set via Scroll), so qdrantLess is the
// one place that ordering logic lives - a pure function over []scored, no
// live qdrant harness needed (same reasoning as TestExcludeInvalidatedFilter).
func TestQdrantLess(t *testing.T) {
	all := []scored{
		{ID: "a", Timestamp: "2026-08-01T00:00:00Z", Upvotes: 1, Downvotes: 5, VoteScore: -4, Recalls: 1, LastRecalledAt: "2026-08-01T00:00:00Z"},
		{ID: "b", Timestamp: "2026-08-02T00:00:00Z", Upvotes: 5, Downvotes: 1, VoteScore: 4, Recalls: 9, LastRecalledAt: "2026-08-03T00:00:00Z"},
		{ID: "c", Timestamp: "2026-08-03T00:00:00Z", Upvotes: 3, Downvotes: 3, VoteScore: 0, Recalls: 5}, // never recalled
	}

	cases := []struct {
		sortBy string
		want   []string // full order, id
	}{
		{SortNewest, []string{"c", "b", "a"}},
		{SortOldest, []string{"a", "b", "c"}},
		{SortScore, []string{"b", "c", "a"}},
		{SortUpvotes, []string{"b", "c", "a"}},
		{SortDownvotes, []string{"a", "c", "b"}},
		{SortRecalls, []string{"b", "c", "a"}},
		// last_recalled: descending by recency, but "c" (never recalled) must
		// sort LAST, not first - a naive string compare would put "" ahead of
		// any RFC3339 timestamp and get this backwards.
		{SortLastRecalled, []string{"b", "a", "c"}},
	}
	for _, tc := range cases {
		cp := append([]scored(nil), all...)
		sort.Slice(cp, qdrantLess(cp, tc.sortBy))
		var got []string
		for _, s := range cp {
			got = append(got, s.ID)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("sort=%s: got %v, want %v", tc.sortBy, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("sort=%s: got %v, want %v", tc.sortBy, got, tc.want)
			}
		}
	}
}

// TestQdrantLess_IDTieBreak: two rows sharing the sort column's value must
// still order deterministically (by ID, descending) - otherwise paging a tie
// through offset/limit could duplicate or drop a row across page boundaries.
func TestQdrantLess_IDTieBreak(t *testing.T) {
	all := []scored{
		{ID: "x", Timestamp: "2026-08-01T00:00:00Z", Upvotes: 2},
		{ID: "y", Timestamp: "2026-08-01T00:00:00Z", Upvotes: 2},
	}
	for _, sortBy := range []string{SortNewest, SortOldest, SortScore, SortUpvotes, SortDownvotes, SortRecalls, SortLastRecalled} {
		cp := append([]scored(nil), all...)
		sort.Slice(cp, qdrantLess(cp, sortBy))
		if cp[0].ID != "y" || cp[1].ID != "x" {
			t.Fatalf("sort=%s: tie-break order = [%s, %s], want [y, x] (ID descending)", sortBy, cp[0].ID, cp[1].ID)
		}
	}
}
