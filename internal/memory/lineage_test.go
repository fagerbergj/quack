package memory

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"google.golang.org/adk/v2/model"
)

func TestParseSurvivorID(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"duplicate of abc-123", "abc-123"},
		{"Duplicate of ABC-123", "ABC-123"},
		{`duplicate of "abc-123"`, "abc-123"},
		{"contradicted by newer info", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseSurvivorID(c.reason); got != c.want {
			t.Errorf("parseSurvivorID(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

func TestComputeAbsorbDelta(t *testing.T) {
	// A absorbed by B: B inherits A's votes and lineage.
	survivor := absorbFields{Upvotes: 1, Downvotes: 0, LastUpvotedAt: "2026-01-01T00:00:00Z"}
	absorbed := absorbFields{Upvotes: 2, Downvotes: 1, LastRecalledAt: "2026-02-01T00:00:00Z"}
	d := computeAbsorbDelta(survivor, absorbed, "A")
	if d.Upvotes != 3 || d.Downvotes != 1 || d.VoteScore != 2 {
		t.Fatalf("votes = %+v, want up=3 down=1 score=2", d)
	}
	if d.Tier != TierVerified {
		t.Fatalf("tier = %q, want verified (upvotes >= 1)", d.Tier)
	}
	if d.LastUpvotedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("last_upvoted_at = %q, want survivor's (absorbed had none)", d.LastUpvotedAt)
	}
	if d.LastRecalledAt != "2026-02-01T00:00:00Z" {
		t.Fatalf("last_recalled_at = %q, want absorbed's (survivor had none)", d.LastRecalledAt)
	}
	if !reflect.DeepEqual(d.AbsorbedIDs, []string{"A"}) {
		t.Fatalf("absorbed_ids = %v, want [A]", d.AbsorbedIDs)
	}

	// Chain: B (already carrying A) absorbed by C - C ends up with both.
	survivorC := absorbFields{}
	absorbedB := absorbFields{AbsorbedIDs: []string{"A"}}
	d2 := computeAbsorbDelta(survivorC, absorbedB, "B")
	sort.Strings(d2.AbsorbedIDs)
	if !reflect.DeepEqual(d2.AbsorbedIDs, []string{"A", "B"}) {
		t.Fatalf("chained absorbed_ids = %v, want [A B]", d2.AbsorbedIDs)
	}
}

func TestComputeAbsorbDelta_ZeroVotesStayUnverified(t *testing.T) {
	d := computeAbsorbDelta(absorbFields{}, absorbFields{}, "x")
	if d.Tier != TierUnverified {
		t.Fatalf("tier = %q, want unverified (no votes on either side)", d.Tier)
	}
}

// seedPoint upserts one point with explicit vote/lineage fields, for tests
// that need to control a memory's pre-absorption state directly.
func seedPoint(t *testing.T, s *Store, p point) {
	t.Helper()
	if p.Vector == nil {
		p.Vector = []float32{1, 0, 0, 0}
	}
	if err := s.idx.upsert(context.Background(), []point{p}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
}

// TestSQLiteAbsorb_VotesAndTimestampsMerge exercises the index-level absorb
// against sqlite (the always-on backend - see store_test.go; qdrant has no
// live harness in this repo, see qdrant_filter_test.go).
func TestSQLiteAbsorb_VotesAndTimestampsMerge(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	seedPoint(t, s, point{ID: "survivor", Scope: "role:coding", Content: "x", Upvotes: 1, VoteScore: 1, LastUpvotedAt: "2026-01-01T00:00:00Z"})
	seedPoint(t, s, point{ID: "dup", Scope: "role:coding", Content: "y", Upvotes: 2, Downvotes: 1, VoteScore: 1, LastRecalledAt: "2026-03-01T00:00:00Z"})

	ok, err := s.idx.absorb(ctx, "survivor", "dup", absorbedByReason("survivor"))
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}
	if !ok {
		t.Fatal("absorb returned false, want true")
	}

	mems, _, err := s.List(ctx, nil, 0, 0, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]Memory{}
	for _, m := range mems {
		byID[m.ID] = m
	}
	sv := byID["survivor"]
	if sv.Upvotes != 3 || sv.Downvotes != 1 || sv.VoteScore != 2 {
		t.Fatalf("survivor votes = +%d/-%d score %d, want +3/-1 score 2", sv.Upvotes, sv.Downvotes, sv.VoteScore)
	}
	if sv.Tier != TierVerified {
		t.Fatalf("survivor tier = %q, want verified", sv.Tier)
	}
	if sv.LastUpvotedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("survivor last_upvoted_at = %q, want carried from survivor", sv.LastUpvotedAt)
	}
	if sv.LastRecalledAt != "2026-03-01T00:00:00Z" {
		t.Fatalf("survivor last_recalled_at = %q, want inherited from absorbed", sv.LastRecalledAt)
	}
	if !reflect.DeepEqual(sv.AbsorbedIDs, []string{"dup"}) {
		t.Fatalf("survivor absorbed_ids = %v, want [dup]", sv.AbsorbedIDs)
	}

	dup := byID["dup"]
	if dup.Status != string(StatusInvalidated) {
		t.Fatalf("absorbed status = %q, want invalidated", dup.Status)
	}
	if dup.InvalidationReason != "absorbed by survivor" {
		t.Fatalf("absorbed invalidation_reason = %q, want %q", dup.InvalidationReason, "absorbed by survivor")
	}
}

// TestSQLiteAbsorb_ChainReproducesSummedVotes proves A absorbed by B absorbed
// by C ends with C carrying BOTH ids and A's votes (folded into B first,
// then B's total - including A's - folded into C).
func TestAbsorb_ChainReproducesSummedVotes(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		aID, bID, cID := testID("A"), testID("B"), testID("C")
		seedPoint(t, s, point{ID: aID, Scope: "role:coding", Content: "a", Upvotes: 1, VoteScore: 1})
		seedPoint(t, s, point{ID: bID, Scope: "role:coding", Content: "b", Upvotes: 1, VoteScore: 1})
		seedPoint(t, s, point{ID: cID, Scope: "role:coding", Content: "c"})

		if ok, err := s.idx.absorb(ctx, bID, aID, absorbedByReason(bID)); err != nil || !ok {
			t.Fatalf("absorb(B,A) = %v, %v", ok, err)
		}
		if ok, err := s.idx.absorb(ctx, cID, bID, absorbedByReason(cID)); err != nil || !ok {
			t.Fatalf("absorb(C,B) = %v, %v", ok, err)
		}

		mems, _, err := s.List(ctx, nil, 0, 0, true, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		byID := map[string]Memory{}
		for _, m := range mems {
			byID[m.ID] = m
		}
		c := byID[cID]
		if c.Upvotes != 2 {
			t.Fatalf("C upvotes = %d, want 2 (A's + B's, chained)", c.Upvotes)
		}
		sort.Strings(c.AbsorbedIDs)
		wantAbsorbed := []string{aID, bID}
		sort.Strings(wantAbsorbed)
		if !reflect.DeepEqual(c.AbsorbedIDs, wantAbsorbed) {
			t.Fatalf("C absorbed_ids = %v, want [A B]", c.AbsorbedIDs)
		}
		if byID[aID].InvalidationReason != absorbedByReason(bID) {
			t.Fatalf("A's own invalidation reason = %q, want unchanged %q (never rewritten to C)", byID[aID].InvalidationReason, absorbedByReason(bID))
		}
		if byID[bID].InvalidationReason != absorbedByReason(cID) {
			t.Fatalf("B invalidation reason = %q, want %q", byID[bID].InvalidationReason, absorbedByReason(cID))
		}
	})
}

// TestSQLiteAbsorb_AlreadyInvalidatedIsNoop: absorbing an id that's already
// invalidated for some other reason must not overwrite that reason (sticky).
func TestSQLiteAbsorb_AlreadyInvalidatedIsNoop(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	seedPoint(t, s, point{ID: "survivor", Scope: "role:coding", Content: "x"})
	seedPoint(t, s, point{ID: "dup", Scope: "role:coding", Content: "y", Status: string(StatusInvalidated), InvalidationReason: "net score"})

	ok, err := s.idx.absorb(ctx, "survivor", "dup", absorbedByReason("survivor"))
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}
	if ok {
		t.Fatal("absorb of an already-invalidated memory should be a no-op")
	}
	mems, _, _ := s.List(ctx, nil, 0, 0, true, "")
	for _, m := range mems {
		if m.ID == "dup" && m.InvalidationReason != "net score" {
			t.Fatalf("dup's original invalidation reason was overwritten: %q", m.InvalidationReason)
		}
	}
}

// TestApplyVotes_DropsForAlreadyAbsorbedID: a vote arriving for an id that
// was already absorbed (invalidated) is dropped, not redirected to the
// survivor - the same sticky "already invalidated" rule every vote/outcome
// path already applies.
func TestApplyVotes_DropsForAlreadyAbsorbedID(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	seedPoint(t, s, point{ID: "survivor", Scope: "role:coding", Content: "x"})
	seedPoint(t, s, point{ID: "dup", Scope: "role:coding", Content: "y"})
	if ok, err := s.idx.absorb(ctx, "survivor", "dup", absorbedByReason("survivor")); err != nil || !ok {
		t.Fatalf("absorb = %v, %v", ok, err)
	}

	touched, err := s.ApplyVotes(ctx, []Vote{{MemoryID: "dup", Vote: VoteSupported}}, DefaultInvalidateThreshold)
	if err != nil {
		t.Fatalf("ApplyVotes: %v", err)
	}
	if touched != 0 {
		t.Fatalf("ApplyVotes touched %d for an absorbed id, want 0 (dropped)", touched)
	}
}
