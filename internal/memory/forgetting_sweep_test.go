package memory

import (
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledgertest"
)

// daysAgo formats an RFC3339 UTC timestamp n days before now, matching
// nowRFC3339's convention (see fieldsFor).
func daysAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour).Format(time.RFC3339)
}

// TestFieldsFor_DaysSinceMintedFallback pins the legacy-row fallback chain (MintedAt -> ValidFrom
// -> Timestamp): a consolidator reword re-stamps Timestamp to now on every UPDATE, so a legacy
// row's age must come from ValidFrom (preserved across UPDATE) when both are present.
func TestFieldsFor_DaysSinceMintedFallback(t *testing.T) {
	p := scored{MintedAt: "", ValidFrom: daysAgo(300), Timestamp: daysAgo(0)}
	f := fieldsFor(p, time.Now().UTC())
	if f.DaysSinceMinted < 299 || f.DaysSinceMinted > 300 {
		t.Errorf("days_since_minted = %d, want ~300 (from ValidFrom, not the fresh Timestamp)", f.DaysSinceMinted)
	}
}

// TestForgetSweep_DefaultRules seeds one memory per default rule (epic #1456 P2's usage-based
// set) plus one that matches none, and proves a dry run reports without mutating while a real
// sweep applies the same matches - invalidate, demote, and keep all included.
func TestForgetSweep_DefaultRules(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)

		neverRecalledID := testID("never-recalled")
		recalledNoSupportID := testID("recalled-no-support")
		badScoreID := testID("bad-score")
		supportDecayedID := testID("support-decayed")
		keptVerifiedID := testID("kept-verified")
		untouchedID := testID("untouched")

		seedMemory(t, s, point{ID: neverRecalledID, Content: "never recalled, minted a month ago", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(31), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified, Recalls: 0})
		seedMemory(t, s, point{ID: recalledNoSupportID, Content: "recalled 3 times, never supported", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(5), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified, Recalls: 3, Supported: 0})
		seedMemory(t, s, point{ID: badScoreID, Content: "downvoted into oblivion", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(5), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified, Recalls: 1, Downvotes: 3, VoteScore: -3})
		seedMemory(t, s, point{ID: supportDecayedID, Content: "verified, no upvote in a year", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(400), ValidFrom: "t",
			Status: string(StatusReinforced), Tier: TierVerified, Upvotes: 1, Supported: 1, VoteScore: 1, LastUpvotedAt: daysAgo(400)})
		seedMemory(t, s, point{ID: keptVerifiedID, Content: "verified, upvoted last week", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(400), ValidFrom: "t",
			Status: string(StatusReinforced), Tier: TierVerified, Upvotes: 1, Supported: 1, VoteScore: 1, LastUpvotedAt: daysAgo(7)})
		seedMemory(t, s, point{ID: untouchedID, Content: "unverified, recalled once, too young to age out", Scope: "repo:r",
			Author: "a", Timestamp: "t", MintedAt: daysAgo(5), ValidFrom: "t",
			Status: string(StatusUnverified), Tier: TierUnverified, Recalls: 1})

		report, err := s.ForgetSweep(ctx, true)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if report.Evaluated != 6 {
			t.Fatalf("evaluated = %d, want 6", report.Evaluated)
		}
		if report.Kept != 1 { // untouched: no rule matches
			t.Errorf("kept = %d, want 1", report.Kept)
		}
		wantMatched := map[int]int{0: 1, 1: 1, 2: 1, 3: 1, 4: 1}
		for _, r := range report.Rules {
			if r.Matched != wantMatched[r.Index] {
				t.Errorf("rule %d matched = %d, want %d", r.Index, r.Matched, wantMatched[r.Index])
			}
		}
		if report.Rules[3].Then != ThenDemote {
			t.Errorf("rule 3 then = %q, want %q", report.Rules[3].Then, ThenDemote)
		}

		// Dry run must not mutate anything.
		assertStatus(t, s, neverRecalledID, string(StatusUnverified))
		assertStatus(t, s, supportDecayedID, string(StatusReinforced))
		assertTier(t, s, supportDecayedID, TierVerified)

		// Real sweep applies the same matches.
		if _, err := s.ForgetSweep(ctx, false); err != nil {
			t.Fatalf("real sweep: %v", err)
		}
		assertStatus(t, s, neverRecalledID, string(StatusInvalidated))
		assertReason(t, s, neverRecalledID, ReasonNeverRecalled)
		assertStatus(t, s, recalledNoSupportID, string(StatusInvalidated))
		assertReason(t, s, recalledNoSupportID, ReasonRecalledWithoutSupport)
		assertStatus(t, s, badScoreID, string(StatusInvalidated))
		assertStatus(t, s, supportDecayedID, string(StatusReinforced)) // demote never invalidates
		assertTier(t, s, supportDecayedID, TierUnverified)
		assertStatus(t, s, keptVerifiedID, string(StatusReinforced))
		assertTier(t, s, keptVerifiedID, TierVerified)
		assertStatus(t, s, untouchedID, string(StatusUnverified)) // untouched

		// Idempotent: invalidated points are excluded from the next sweep's currently-valid
		// page, and the demoted point no longer matches its own rule (tier is now unverified),
		// so a second run changes nothing.
		report2, err := s.ForgetSweep(ctx, false)
		if err != nil {
			t.Fatalf("second sweep: %v", err)
		}
		for _, r := range report2.Rules {
			if r.Matched != 0 && r.Then != ThenKeep {
				t.Errorf("second sweep rule %d matched %d, want 0 (idempotent)", r.Index, r.Matched)
			}
		}
	})
}

// TestForgetSweep_RepeatedRoundRecallDoesNotTripRule1 covers #1470: rule 1
// (`unverified && recalls >= 3 && supported == 0` -> invalidate) must not fire from a
// single unvoted round's repeat calls - only the round-deduped counter feeds it.
func TestForgetSweep_RepeatedRoundRecallDoesNotTripRule1(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		id := testID("recalled-thrice-one-round")
		seedMemory(t, s, point{ID: id, Content: "c", Scope: "repo:r", Author: "a", Timestamp: "t",
			MintedAt: daysAgo(5), ValidFrom: "t", Status: string(StatusUnverified), Tier: TierUnverified})

		// One unvoted round: a worker calls recall_memory three times for this memory
		// before the judge ever votes - the caller-side round dedup (vetting's
		// mergeMemoryHits) lets only the first call's id through to RecordRecall.
		lgr := ledgertest.NewMemStore()
		hits := []Delivered{{ID: id, Content: "c"}}
		roundSeen := map[string]bool{}
		for i := 0; i < 3; i++ {
			s.LogRecallLedgerOnly(ctx, lgr, "chat1", "node1", "tool", hits)
			if !roundSeen[id] {
				roundSeen[id] = true
				s.RecordRecall(ctx, []string{id})
			}
		}

		report, err := s.ForgetSweep(ctx, false)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if report.Rules[1].Matched != 0 {
			t.Fatalf("rule 1 matched = %d, want 0 (recalls stayed at 1, not 3, for one round's repeats)", report.Rules[1].Matched)
		}
		assertStatus(t, s, id, string(StatusUnverified))
	})
}

// TestForgetSweep_Demote covers demote end to end on both backends: tier flips, status/score
// stay untouched, one memory_ops row lands, and a second sweep neither re-demotes the now-
// unverified point nor invalidates it as "never recalled" (rule 0's supported==0 guard).
func TestForgetSweep_Demote(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		ops := &fakeOpsLog{}
		s.SetOpsLog(ops)

		staleID := testID("stale-verified")
		seedMemory(t, s, point{ID: staleID, Content: "c", Scope: "repo:r", Author: "a", Timestamp: "t",
			MintedAt: daysAgo(300), ValidFrom: "t", Status: string(StatusReinforced),
			Tier: TierVerified, Upvotes: 1, Supported: 1, VoteScore: 1, LastUpvotedAt: daysAgo(200)})

		if _, err := s.ForgetSweep(ctx, false); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		assertTier(t, s, staleID, TierUnverified)
		assertStatus(t, s, staleID, string(StatusReinforced)) // demote never touches status

		ops.mu.Lock()
		if len(ops.rows) != 1 {
			t.Fatalf("memory_ops rows = %d, want 1", len(ops.rows))
		}
		row := ops.rows[0]
		ops.mu.Unlock()
		if row.memoryID != staleID || row.op != OpDemote || row.actor != ActorConsolidator || row.reason != ReasonSupportDecayed {
			t.Errorf("op row = %+v, want id=%s op=demote actor=consolidator reason=%q", row, staleID, ReasonSupportDecayed)
		}

		// A second sweep must neither re-demote (rule 3 needs tier verified) nor invalidate the
		// now-unverified, never-recalled, 300-day-old row as "never recalled" (rule 0's guard).
		if _, err := s.ForgetSweep(ctx, false); err != nil {
			t.Fatalf("second sweep: %v", err)
		}
		assertStatus(t, s, staleID, string(StatusReinforced))
		assertTier(t, s, staleID, TierUnverified)
		ops.mu.Lock()
		gotRows := len(ops.rows)
		ops.mu.Unlock()
		if gotRows != 1 {
			t.Errorf("memory_ops rows after second sweep = %d, want 1 (no re-demote, no invalidate)", gotRows)
		}
	})
}

// TestDemoteTier_NoOpOnUnverified drives the index's demoteTier and Store.demoteByRule directly
// against an already-unverified point - the sweep's own rule shape never routes an unverified row
// to demoteTier, so a rule-engine-driven test can't exercise this guard on both backends.
func TestDemoteTier_NoOpOnUnverified(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		s := newStore("task", nil)
		ops := &fakeOpsLog{}
		s.SetOpsLog(ops)

		id := testID("already-unverified")
		seedMemory(t, s, point{ID: id, Content: "c", Scope: "repo:r", Author: "a", Timestamp: "t",
			MintedAt: daysAgo(5), ValidFrom: "t", Status: string(StatusUnverified), Tier: TierUnverified})

		touched, err := s.idx.demoteTier(ctx, []string{id})
		if err != nil {
			t.Fatalf("demoteTier: %v", err)
		}
		if len(touched) != 0 {
			t.Errorf("touched = %v, want none", touched)
		}

		s.demoteByRule(ctx, []string{id})
		ops.mu.Lock()
		gotRows := len(ops.rows)
		ops.mu.Unlock()
		if gotRows != 0 {
			t.Errorf("memory_ops rows = %d, want 0 (no-op writes no row)", gotRows)
		}
		assertTier(t, s, id, TierUnverified)
	})
}

func assertTier(t *testing.T, s *Store, id, want string) {
	t.Helper()
	mems, _, err := s.List(context.Background(), nil, 0, 100, true, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range mems {
		if m.ID == id {
			tier := m.Tier
			if tier == "" {
				tier = TierUnverified
			}
			if tier != want {
				t.Errorf("%s tier = %q, want %q", id, tier, want)
			}
			return
		}
	}
	t.Fatalf("memory %s not found", id)
}

func assertReason(t *testing.T, s *Store, id, want string) {
	t.Helper()
	mems, _, err := s.List(context.Background(), nil, 0, 100, true, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range mems {
		if m.ID == id {
			if m.InvalidationReason != want {
				t.Errorf("%s invalidation_reason = %q, want %q", id, m.InvalidationReason, want)
			}
			return
		}
	}
	t.Fatalf("memory %s not found", id)
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
	if row.reason != "rule 2: score <= -2" {
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
