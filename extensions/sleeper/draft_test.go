package sleeper

import (
	"context"
	"fmt"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

func TestGetDraft(t *testing.T) {
	e := testExtension(t)
	got, err := e.getDraft(context.Background(), draftArgs{LeagueID: testLeague})
	if err != nil {
		t.Fatalf("getDraft: %v", err)
	}
	if got.Status != "complete" {
		t.Errorf("status = %q, want complete", got.Status)
	}
	if len(got.Picks) != 140 {
		t.Errorf("picks = %d, want 140", len(got.Picks))
	}
	if len(got.Order) != 10 {
		t.Errorf("order = %d, want 10", len(got.Order))
	}
	if got.OnClock != nil {
		t.Errorf("on_clock = %+v, want nil for a completed draft", got.OnClock)
	}
	first := got.Picks[0]
	if first.PickNo != 1 || first.PlayerID != "9221" {
		t.Errorf("first pick = %+v, want pick 1 / player 9221 (Jahmyr Gibbs)", first)
	}
	// The ADP sentinel: a player with no real draft consensus reads 999/1000,
	// never a literal 999th-overall pick.
	var sawNoADP bool
	for _, p := range got.Picks {
		if p.NoADP && p.ADP < noADPSentinel {
			t.Errorf("pick %+v: no_adp=true but adp=%v < sentinel", p, p.ADP)
		}
		if p.NoADP {
			sawNoADP = true
		}
	}
	if !sawNoADP {
		t.Error("expected at least one drafted player with no ADP")
	}
}

func TestOnClockSnakeMath(t *testing.T) {
	names := map[int]string{1: "a", 2: "b", 3: "c"}
	slotToRoster := map[string]int{"1": 1, "2": 2, "3": 3}
	d := &sleepergen.Draft{
		Status: "drafting", Type: "snake",
		Settings:       map[string]int{"teams": 3, "rounds": 2},
		SlotToRosterId: &slotToRoster,
	}
	cases := []struct {
		picksMade  int
		wantSlot   int
		wantRoster int
	}{
		{0, 1, 1}, // round 1, pick 1: slot 1
		{2, 3, 3}, // round 1, pick 3: slot 3
		{3, 3, 3}, // round 2, pick 4: snake reverses, slot 3 goes first
		{5, 1, 1}, // round 2, pick 6: slot 1 goes last
	}
	for _, tc := range cases {
		got := onClock(d, 2, tc.picksMade, names)
		if got == nil {
			t.Fatalf("picksMade=%d: onClock = nil, want a pick", tc.picksMade)
		}
		if got.Slot != tc.wantSlot || got.RosterID != tc.wantRoster {
			t.Errorf("picksMade=%d: got slot=%d roster=%d, want slot=%d roster=%d", tc.picksMade, got.Slot, got.RosterID, tc.wantSlot, tc.wantRoster)
		}
	}
	if got := onClock(d, 2, 6, names); got != nil {
		t.Errorf("draft complete (6/6 picks): onClock = %+v, want nil", got)
	}
}

func TestOnClockNilWhenNotDrafting(t *testing.T) {
	slotToRoster := map[string]int{"1": 1}
	d := &sleepergen.Draft{Status: "complete", Type: "snake", SlotToRosterId: &slotToRoster}
	if got := onClock(d, 1, 0, nil); got != nil {
		t.Errorf("onClock = %+v, want nil for a non-drafting status", got)
	}
}

// TestOnClockFallsBackToRosterSizeWhenRoundsUnset: unset settings.rounds must not make every draft look done.
func TestOnClockFallsBackToRosterSizeWhenRoundsUnset(t *testing.T) {
	names := map[int]string{1: "a", 2: "b"}
	slotToRoster := map[string]int{"1": 1, "2": 2}
	d := &sleepergen.Draft{
		Status: "drafting", Type: "snake",
		Settings:       map[string]int{"teams": 2}, // no "rounds" key
		SlotToRosterId: &slotToRoster,
	}
	if got := onClock(d, 0, 0, names); got != nil {
		t.Errorf("rounds=0 with no fallback given: onClock = %+v, want nil", got)
	}
	got := onClock(d, 3, 0, names) // caller derived rounds=3 from roster size
	if got == nil || got.Slot != 1 {
		t.Errorf("onClock(rounds=3, picksMade=0) = %+v, want slot 1", got)
	}
}

// TestPickDraftPrefersSeasonMatch: with two drafts, the season match wins regardless of start_time.
func TestPickDraftPrefersSeasonMatch(t *testing.T) {
	old := int(1000)
	newer := int(2000)
	drafts := []sleepergen.Draft{
		{DraftId: "old-redraft", Season: "2026", StartTime: &newer},
		{DraftId: "right-season", Season: "2025", StartTime: &old},
	}
	got := pickDraft(drafts, "2025")
	if got.DraftId != "right-season" {
		t.Errorf("pickDraft = %q, want right-season (season match beats a later start_time)", got.DraftId)
	}
}

// TestPickDraftFallsBackToLatestStartTime covers no season match: the
// most recently started draft wins.
func TestPickDraftFallsBackToLatestStartTime(t *testing.T) {
	early := int(1000)
	late := int(2000)
	drafts := []sleepergen.Draft{
		{DraftId: "earlier", Season: "2024", StartTime: &early},
		{DraftId: "later", Season: "2023", StartTime: &late},
	}
	got := pickDraft(drafts, "2026")
	if got.DraftId != "later" {
		t.Errorf("pickDraft = %q, want later (no season match, latest start_time wins)", got.DraftId)
	}
}

// TestDraftRoundsFallsBackToStartingSlotCount: no settings.rounds falls back to the starter-slot count,
// not the full 14-slot roster.
func TestDraftRoundsFallsBackToStartingSlotCount(t *testing.T) {
	e := testExtension(t)
	league, err := e.client.League(context.Background(), testLeague)
	if err != nil {
		t.Fatalf("League: %v", err)
	}
	if len(league.RosterPositions) != 14 {
		t.Fatalf("fixture league roster_positions = %d, want 14 (test assumption changed)", len(league.RosterPositions))
	}
	d := &sleepergen.Draft{LeagueId: testLeague, Settings: map[string]int{"teams": 10}}
	got := e.draftRounds(context.Background(), d)
	if got != 9 {
		t.Errorf("draftRounds = %d, want 9 (len(nonBenchSlots), not the 14-slot full roster)", got)
	}
	d.Settings["rounds"] = 12
	if got := e.draftRounds(context.Background(), d); got != 12 {
		t.Errorf("draftRounds = %d, want 12 (settings.rounds takes priority)", got)
	}
}

// TestADPDeltaAndVerdict checks pick_no - adp and the value/reach verdict on real prod-grader shapes.
func TestADPDeltaAndVerdict(t *testing.T) {
	cases := []struct {
		name        string
		pickNo      int
		adp         float32
		noADP       bool
		wantDelta   *float32
		wantVerdict string
	}{
		{"Loveland reach: pick 35 vs ADP 106", 35, 106, false, f32ptr(-71), "reach"},
		{"Hubbard value: pick 86 vs ADP 50", 86, 50, false, f32ptr(36), "value"},
		{"fair: within the 8-pick band", 20, 15, false, f32ptr(5), "fair"},
		{"missing ADP: delta and verdict both omitted", 12, 999, true, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDelta, gotVerdict := adpDeltaAndVerdict(tc.pickNo, tc.adp, tc.noADP)
			if (gotDelta == nil) != (tc.wantDelta == nil) || (gotDelta != nil && *gotDelta != *tc.wantDelta) {
				t.Errorf("delta = %v, want %v", f32str(gotDelta), f32str(tc.wantDelta))
			}
			if gotVerdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", gotVerdict, tc.wantVerdict)
			}
		})
	}
}

func f32ptr(v float32) *float32 { return &v }

func f32str(v *float32) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *v)
}

func TestBestAvailableExcludesPickedAndCapsPerPosition(t *testing.T) {
	dump := playerDumpAt("rb1", "RB", "rb2", "RB", "rb3", "RB", "qb1", "QB")
	proj := map[string]sleepergen.StatMap{
		"rb1": {"pts_ppr": 20}, "rb2": {"pts_ppr": 15}, "rb3": {"pts_ppr": 10}, "qb1": {"pts_ppr": 25},
	}
	picks := []sleepergen.DraftPick{{PlayerId: "rb1"}}
	got := bestAvailable(dump, picks, proj)
	rbs := got["RB"]
	if len(rbs) != 2 {
		t.Fatalf("RB best available = %+v, want 2 (rb1 picked)", rbs)
	}
	if rbs[0].PlayerID != "rb2" || rbs[1].PlayerID != "rb3" {
		t.Errorf("RB order = %+v, want [rb2, rb3] by projection desc", rbs)
	}
	if len(got["QB"]) != 1 || got["QB"][0].PlayerID != "qb1" {
		t.Errorf("QB best available = %+v, want [qb1]", got["QB"])
	}
	if len(got["WR"]) != 0 {
		t.Errorf("WR best available = %+v, want none in the dump", got["WR"])
	}
}
