package memory

import (
	"sort"
	"testing"
)

// qdrant's list() sorts entirely in Go, so qdrantLess is the only place ordering lives; a pure function,
// no live qdrant needed.
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
		// "c" (never recalled) must sort last; a naive string compare puts "" ahead of any timestamp.
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

// Ties on the sort column order by ID descending, else offset paging could duplicate or drop a row.
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
