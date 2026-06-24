package memory

import (
	"context"
	"math"
	"testing"
)

// TestSQLiteIndex exercises the brute-force backend directly (the shared Store
// tests use an all-equal fakeEmbedder, which can't show ranking): real cosine
// ordering, UPDATE-overwrites-in-place, DELETE, and scope isolation.
func TestSQLiteIndex(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil) // s.idx is a *sqliteIndex

	if err := s.idx.upsert(ctx, []point{
		{ID: "a", Vector: []float32{1, 0, 0}, Content: "near", Scope: "x"},
		{ID: "b", Vector: []float32{0.8, 0.2, 0}, Content: "mid", Scope: "x"},
		{ID: "c", Vector: []float32{0, 1, 0}, Content: "far", Scope: "x"},
		{ID: "z", Vector: []float32{1, 0, 0}, Content: "other-scope", Scope: "y"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Query near {1,0,0} in scope x → a, b, c by descending cosine; scope y excluded.
	got, err := s.idx.query(ctx, "x", []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d hits, want 3 (scope y must be excluded)", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("cosine order = %s,%s,%s, want a,b,c", got[0].ID, got[1].ID, got[2].ID)
	}
	if !(got[0].Score > got[1].Score && got[1].Score > got[2].Score) {
		t.Fatalf("scores not strictly descending: %v", []float32{got[0].Score, got[1].Score, got[2].Score})
	}

	// k caps the result count.
	if top, _ := s.idx.query(ctx, "x", []float32{1, 0, 0}, 1); len(top) != 1 || top[0].ID != "a" {
		t.Fatalf("k=1 → %+v, want just a", top)
	}

	// UPDATE: same id overwrites in place (no duplicate row).
	if err := s.idx.upsert(ctx, []point{{ID: "a", Vector: []float32{1, 0, 0}, Content: "updated", Scope: "x"}}); err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	got, _ = s.idx.query(ctx, "x", []float32{1, 0, 0}, 5)
	if len(got) != 3 || got[0].ID != "a" || got[0].Content != "updated" {
		t.Fatalf("after UPDATE: %d rows, top=%+v, want 3 rows with a.Content=updated", len(got), got[0])
	}

	// DELETE removes only the named id.
	if err := s.idx.remove(ctx, []string{"a"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got, _ = s.idx.query(ctx, "x", []float32{1, 0, 0}, 5); len(got) != 2 {
		t.Fatalf("after DELETE: %d rows, want 2", len(got))
	}
}

func TestCosineAndVecRoundTrip(t *testing.T) {
	// Identical → 1, orthogonal → 0, length mismatch / empty → 0.
	if c := cosine([]float32{1, 2, 3}, []float32{1, 2, 3}); math.Abs(float64(c)-1) > 1e-6 {
		t.Errorf("cosine(v,v) = %v, want 1", c)
	}
	if c := cosine([]float32{1, 0}, []float32{0, 1}); c != 0 {
		t.Errorf("cosine(orthogonal) = %v, want 0", c)
	}
	if c := cosine([]float32{1, 0, 0}, []float32{1, 0}); c != 0 {
		t.Errorf("cosine(mismatched len) = %v, want 0", c)
	}

	// BLOB encode/decode is lossless.
	v := []float32{-1.5, 0, 3.25, 1e9}
	if got := bytesToVec(vecToBytes(v)); len(got) != len(v) {
		t.Fatalf("round-trip len %d, want %d", len(got), len(v))
	} else {
		for i := range v {
			if got[i] != v[i] {
				t.Errorf("round-trip[%d] = %v, want %v", i, got[i], v[i])
			}
		}
	}
}
