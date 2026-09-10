package memory

import (
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
)

// daysAgo formats an RFC3339 UTC timestamp n days before now, matching
// nowRFC3339's convention (see fieldsFor).
func daysAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

// TestForgetSweep_DefaultRules seeds the three scenarios the epic's P3 verification names
// directly: a verified memory kept despite no recent recall, an old unverified memory
// invalidated for lack of an upvote, and a net-score -2 memory invalidated regardless of tier.
func TestForgetSweep_DefaultRules(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)

		verifiedOldID, unverifiedStaleID := testID("verified-old"), testID("unverified-stale")
		badScoreID, freshUnverifiedID := testID("bad-score"), testID("fresh-unverified")
		seedMemory(t, s, point{ID: verifiedOldID, Content: "verified, no recalls in a year", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(400), ValidFrom: "t",
			Status: string(StatusReinforced), Tier: TierVerified, Upvotes: 1, VoteScore: 1, LastUpvotedAt: daysAgo(400)})
		seedMemory(t, s, point{ID: unverifiedStaleID, Content: "unverified, never upvoted, 91 days old", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(91), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified})
		seedMemory(t, s, point{ID: badScoreID, Content: "downvoted into oblivion", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(5), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified, Downvotes: 2, VoteScore: -2})
		seedMemory(t, s, point{ID: freshUnverifiedID, Content: "unverified but only 3 days old", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(3), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified})

		report, err := s.ForgetSweep(ctx, true)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if report.Evaluated != 4 {
			t.Fatalf("evaluated = %d, want 4", report.Evaluated)
		}
		if report.Kept != 1 { // fresh-unverified: no rule matches
			t.Errorf("kept = %d, want 1", report.Kept)
		}
		wantMatched := map[int]int{0: 1, 1: 1, 2: 1} // rule0=unverified-stale, rule1=bad-score, rule2=verified-old
		for _, r := range report.Rules {
			if r.Matched != wantMatched[r.Index] {
				t.Errorf("rule %d matched = %d, want %d", r.Index, r.Matched, wantMatched[r.Index])
			}
		}

		// Dry run must not mutate anything.
		assertStatus(t, s, verifiedOldID, string(StatusReinforced))
		assertStatus(t, s, unverifiedStaleID, string(StatusUnverified))
		assertStatus(t, s, badScoreID, string(StatusUnverified))

		// Real sweep applies the same matches.
		if _, err := s.ForgetSweep(ctx, false); err != nil {
			t.Fatalf("real sweep: %v", err)
		}
		assertStatus(t, s, verifiedOldID, string(StatusReinforced)) // kept
		assertStatus(t, s, unverifiedStaleID, string(StatusInvalidated))
		assertStatus(t, s, badScoreID, string(StatusInvalidated))
		assertStatus(t, s, freshUnverifiedID, string(StatusUnverified)) // untouched

		// Idempotent: invalidated points are excluded from the next sweep's
		// currently-valid page, so a second run invalidates nothing new.
		report2, err := s.ForgetSweep(ctx, false)
		if err != nil {
			t.Fatalf("second sweep: %v", err)
		}
		if report2.Evaluated != 2 { // verified-old + fresh-unverified only
			t.Errorf("second sweep evaluated = %d, want 2", report2.Evaluated)
		}
		for _, r := range report2.Rules {
			if r.Matched != 0 && r.Then == ThenInvalidate {
				t.Errorf("second sweep rule %d matched %d, want 0 (idempotent)", r.Index, r.Matched)
			}
		}
	})
}

// TestForgetSweep_EmptyScope proves a sweep over a store with no memories at
// all is a no-op, not a panic or error.
func TestForgetSweep_EmptyScope(t *testing.T) {
	s := newSQLiteStore(t, "task", nil)
	report, err := s.ForgetSweep(context.Background(), false)
	if err != nil {
		t.Fatalf("empty sweep: %v", err)
	}
	if report.Evaluated != 0 || report.Kept != 0 {
		t.Errorf("empty sweep report = %+v, want all zero", report)
	}
}

// TestForgetSweep_OpsLog checks the invalidation writes one memory_ops row
// with actor "sweep" and a reason naming the rule index and expression.
func TestForgetSweep_OpsLog(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", nil)
	ops := &fakeOpsLog{}
	s.SetOpsLog(ops)
	seedMemory(t, s, point{ID: "m1", Content: "c", Scope: "repo:r", Author: "a", Timestamp: "t",
		MintedAt: daysAgo(5), ValidFrom: "t", Status: string(StatusUnverified), VoteScore: -3})

	if _, err := s.ForgetSweep(ctx, false); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	ops.mu.Lock()
	defer ops.mu.Unlock()
	if len(ops.rows) != 1 {
		t.Fatalf("memory_ops rows = %d, want 1", len(ops.rows))
	}
	row := ops.rows[0]
	if row.memoryID != "m1" || row.op != OpInvalidate || row.actor != ActorSweep {
		t.Errorf("op row = %+v, want id=m1 op=invalidate actor=sweep", row)
	}
	if row.reason != "rule 1: score <= -2" {
		t.Errorf("reason = %q, want the score rule's index+expression", row.reason)
	}
}

// TestForgetSweep_CustomRules exercises SetForgettingRules end-to-end,
// including rejecting a bad rule at wiring time.
func TestForgetSweep_CustomRules(t *testing.T) {
	s := newSQLiteStore(t, "task", nil)
	if err := s.SetForgettingRules([]Rule{{When: "recalls == 0", Then: ThenInvalidate}}); err != nil {
		t.Fatalf("SetForgettingRules: %v", err)
	}
	seedMemory(t, s, point{ID: "never-recalled", Content: "c", Scope: "repo:r", Author: "a",
		Timestamp: "t", MintedAt: daysAgo(1), ValidFrom: "t", Status: string(StatusUnverified)})

	report, err := s.ForgetSweep(context.Background(), false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if report.Rules[0].Matched != 1 {
		t.Errorf("custom rule matched = %d, want 1", report.Rules[0].Matched)
	}
	assertStatus(t, s, "never-recalled", string(StatusInvalidated))

	if err := s.SetForgettingRules([]Rule{{When: "bogus_field == 1", Then: ThenInvalidate}}); err == nil {
		t.Error("expected SetForgettingRules to reject an unknown field")
	}
}

// TestSweepOnce_ForgetThenRetain covers epic #1255 P3 verification (f): the forgetting step
// invalidates a stale memory, then retentionOnce hard-removes it once its (backdated)
// invalidation would be past the window - proving forgetOnce runs before retentionOnce in sweepOnce, not just each in isolation.
func TestSweepOnce_ForgetThenRetain(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)

		// Minted long enough ago that the forgetting sweep invalidates it today;
		// invalidated_at will be stamped "now", so retention won't remove it yet.
		agesOutID := testID("ages-out")
		seedMemory(t, s, point{ID: agesOutID, Content: "c", Scope: "repo:r", Author: "a", Timestamp: "t",
			MintedAt: daysAgo(91), ValidFrom: "t", Status: string(StatusUnverified)})

		s.sweepOnce(ctx, 30) // forget, then retention with a 30-day window
		assertStatus(t, s, agesOutID, string(StatusInvalidated))

		remaining, _, err := s.List(ctx, nil, 0, 10, true, "")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		found := false
		for _, m := range remaining {
			if m.ID == agesOutID {
				found = true
			}
		}
		if !found {
			t.Fatal("retention removed a memory invalidated in the same tick - it should survive until its own window passes")
		}
	})
}

func assertStatus(t *testing.T, s *Store, id, want string) {
	t.Helper()
	mems, _, err := s.List(context.Background(), nil, 0, 100, true, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range mems {
		if m.ID == id {
			status := m.Status
			if status == "" {
				status = string(StatusUnverified)
			}
			if status != want {
				t.Errorf("%s status = %q, want %q", id, status, want)
			}
			return
		}
	}
	t.Fatalf("memory %s not found", id)
}
