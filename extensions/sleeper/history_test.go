package sleeper

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// TestGetHistoryKnownValues pins numbers checked by hand against the fixtures: Barkley drafted RB3
// finished RB14, 88.5% 2025 efficiency, 1 close loss.
func TestGetHistoryKnownValues(t *testing.T) {
	e := testExtension(t)
	got, err := e.getHistory(context.Background(), historyArgs{})
	if err != nil {
		t.Fatalf("getHistory: %v", err)
	}
	if len(got.Seasons) != 2 {
		t.Fatalf("seasons = %d, want 2 (2026, 2025 - Chain stops at the recorded hop)", len(got.Seasons))
	}
	var season2025 *seasonHistory
	for i := range got.Seasons {
		if got.Seasons[i].Season == "2025" {
			season2025 = &got.Seasons[i]
		}
	}
	if season2025 == nil {
		t.Fatal("expected a 2025 season in the chain")
	}
	if season2025.LineupEfficiencyPct != 88.5 {
		t.Errorf("2025 lineup_efficiency_pct = %v, want 88.5", season2025.LineupEfficiencyPct)
	}
	var barkley *draftReportCardEntry
	for i := range season2025.Draft {
		if season2025.Draft[i].PlayerID == "4866" {
			barkley = &season2025.Draft[i]
		}
	}
	if barkley == nil {
		t.Fatal("expected Saquon Barkley (4866) in the 2025 draft report card")
	}
	if barkley.DraftedAsRank != 3 {
		t.Errorf("Barkley drafted_as_rank = %d, want 3 (RB3)", barkley.DraftedAsRank)
	}
	if barkley.FinishRank != 14 {
		t.Errorf("Barkley finish_rank = %d, want 14 (RB14)", barkley.FinishRank)
	}
	if barkley.Verdict != "reach" {
		t.Errorf("Barkley verdict = %q, want reach (RB3 finishing RB14)", barkley.Verdict)
	}
	// 2025 wk14 (margin 1.46) is close; 2026 wk1 (margin 15.34) is not, and
	// 2026 wk2 is in progress and excluded entirely.
	if got.TotalCloseLosses != 1 {
		t.Errorf("total_close_losses = %d, want 1", got.TotalCloseLosses)
	}
	for _, sh := range got.Seasons {
		for _, w := range sh.Weeks {
			if sh.Season == "2026" && w.Week >= 2 {
				t.Errorf("season 2026 week %+v: in-progress weeks must be excluded from hindsight", w)
			}
		}
	}
}

func TestGetHistorySeasonsBackCapsChain(t *testing.T) {
	e := testExtension(t)
	got, err := e.getHistory(context.Background(), historyArgs{SeasonsBack: 1})
	if err != nil {
		t.Fatalf("getHistory: %v", err)
	}
	if len(got.Seasons) != 1 {
		t.Fatalf("seasons = %d, want 1", len(got.Seasons))
	}
	if got.Seasons[0].Season != "2026" {
		t.Errorf("season = %q, want 2026 (the current/most recent)", got.Seasons[0].Season)
	}
}

func TestWeekResultCloseLossFixedMargin(t *testing.T) {
	for _, tc := range []struct {
		mine, opp float32
		wantClose bool
	}{
		{134.48, 135.94, true},  // 2025 wk14: 1.46 margin, close
		{100.66, 116.00, false}, // 2026 wk1: 15.34 margin, not close
		{112.14, 108.86, false}, // a win is never a "close loss"
	} {
		result, close := weekResult(tc.mine, tc.opp)
		if close != tc.wantClose {
			t.Errorf("weekResult(%v, %v) close = %v, want %v (result %s)", tc.mine, tc.opp, close, tc.wantClose, result)
		}
	}
}

func TestBestLineupPointsFlexPicksHighestRemaining(t *testing.T) {
	dump := playerDumpAt("rb1", "RB", "rb2", "RB", "rb3", "RB", "wr1", "WR", "wr2", "WR", "wr3", "WR", "te1", "TE")
	points := map[string]float32{
		"rb1": 20, "rb2": 15, "rb3": 12,
		"wr1": 18, "wr2": 10, "wr3": 8,
		"te1": 5,
	}
	ids := []string{"rb1", "rb2", "rb3", "wr1", "wr2", "wr3", "te1"}
	positions := []string{"RB", "RB", "WR", "WR", "TE", "FLEX", "BN"}
	got := bestLineup(positions, ids, points, dump).total
	// RB1+RB2 (35) + WR1+WR2 (28) + TE1 (5) + FLEX best remaining (RB3=12 > WR3=8) = 80
	want := float32(80)
	if got != want {
		t.Errorf("bestLineup total = %v, want %v", got, want)
	}
}

// TestBestLineupPointsFlexReachesPositionWithNoStrictSlot is quack's probe:
// TE has no strict slot here, only FLEX, so it must still reach the flex solver.
func TestBestLineupPointsFlexReachesPositionWithNoStrictSlot(t *testing.T) {
	dump := playerDumpAt("qb1", "QB", "rb1", "RB", "wr1", "WR", "te1", "TE")
	points := map[string]float32{"qb1": 20, "rb1": 15, "wr1": 18, "te1": 40}
	ids := []string{"qb1", "rb1", "wr1", "te1"}
	positions := []string{"QB", "RB", "WR", "FLEX", "BN"}
	got := bestLineup(positions, ids, points, dump).total
	want := float32(93) // 20 + 15 + 18 + FLEX picks te1 (40), not 0
	if got != want {
		t.Errorf("bestLineup total = %v, want %v", got, want)
	}
}

// TestBestLineupPointsAllFlexLineup is quack's second probe: no strict
// slots at all, so counts is empty and every candidate must still be seeded.
func TestBestLineupPointsAllFlexLineup(t *testing.T) {
	dump := playerDumpAt("rb1", "RB", "wr1", "WR")
	points := map[string]float32{"rb1": 12, "wr1": 9}
	ids := []string{"rb1", "wr1"}
	positions := []string{"FLEX", "FLEX", "BN"}
	got := bestLineup(positions, ids, points, dump).total
	want := float32(21) // rb1 + wr1, not 0
	if got != want {
		t.Errorf("bestLineup total = %v, want %v", got, want)
	}
}

// TestBestLineupPointsCrossingFlexKindsIsExact: narrowest-first greedy scores 94 (RB2 loses its slot to a
// WR REC_FLEX could take); the exact assignment scores 96.
func TestBestLineupPointsCrossingFlexKindsIsExact(t *testing.T) {
	dump := playerDumpAt("qb1", "QB", "wr1", "WR", "wr2", "WR", "te1", "TE", "rb1", "RB")
	points := map[string]float32{"qb1": 33, "wr1": 20, "wr2": 5, "te1": 36, "rb1": 2}
	ids := []string{"qb1", "wr1", "wr2", "te1", "rb1"}
	positions := []string{"TE", "WRRB_FLEX", "REC_FLEX", "REC_FLEX", "SUPER_FLEX"}
	got := bestLineup(positions, ids, points, dump).total
	want := float32(96)
	if got != want {
		t.Errorf("bestLineup total = %v, want %v (every player has a home: TE->TE, QB->SUPER_FLEX, RB->WRRB_FLEX, both WRs->REC_FLEX)", got, want)
	}
}

// TestBestLineupPointsMatchesBruteForce checks the DP solver against brute force on small random rosters
// (up to 5 flex-eligible players, 3 flex slots).
func TestBestLineupPointsMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	positions := []string{"QB", "RB", "WR", "TE"}
	kinds := []string{"WRRB_FLEX", "REC_FLEX", "FLEX", "SUPER_FLEX"}
	for trial := 0; trial < 200; trial++ {
		n := 1 + rng.Intn(5)
		var ids []string
		dumpArgs := []string{}
		points := map[string]float32{}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("p%d", i)
			pos := positions[rng.Intn(len(positions))]
			ids = append(ids, id)
			dumpArgs = append(dumpArgs, id, pos)
			points[id] = float32(rng.Intn(41))
		}
		dump := playerDumpAt(dumpArgs...)
		slotCount := 1 + rng.Intn(3)
		var slotPositions []string
		for i := 0; i < slotCount; i++ {
			slotPositions = append(slotPositions, kinds[rng.Intn(len(kinds))])
		}
		got := bestLineup(slotPositions, ids, points, dump).total
		want := bruteForceFlexAssignment(slotPositions, ids, dumpArgs, points)
		if got != want {
			t.Fatalf("trial %d: slots=%v ids=%v points=%v: bestLineup total = %v, want %v (brute force)", trial, slotPositions, ids, points, got, want)
		}
		if msg := assignmentProblem(slotPositions, bestLineup(slotPositions, ids, points, dump), points, dump); msg != "" {
			t.Fatalf("trial %d: slots=%v ids=%v points=%v: %s", trial, slotPositions, ids, points, msg)
		}
	}
}

// assignmentProblem checks the reconstructed per-slot lineup itself: eligible, no repeats, sums to the total.
func assignmentProblem(slots []string, a lineupAssignment, points map[string]float32, dump map[string]sleepergen.Player) string {
	seen := map[string]bool{}
	var sum float32
	for i, pid := range a.bySlot {
		if pid == "" {
			continue
		}
		if seen[pid] {
			return fmt.Sprintf("player %s assigned twice: %v", pid, a.bySlot)
		}
		seen[pid] = true
		pos := *dump[pid].Position
		if !slices.Contains(flexKindEligible[slots[i]], pos) && pos != slots[i] {
			return fmt.Sprintf("player %s (%s) not eligible for slot %s", pid, pos, slots[i])
		}
		sum += points[pid]
	}
	if sum != a.total {
		return fmt.Sprintf("assigned points sum %v != total %v", sum, a.total)
	}
	return ""
}

// bruteForceFlexAssignment enumerates every subset+permutation of players
// into slots (no strict slots here, so it only has to solve the flex side).
func bruteForceFlexAssignment(slotPositions, ids, dumpArgs []string, points map[string]float32) float32 {
	posOf := map[string]string{}
	for i := 0; i+1 < len(dumpArgs); i += 2 {
		posOf[dumpArgs[i]] = dumpArgs[i+1]
	}
	used := make([]bool, len(ids))
	var best float32
	var rec func(slotIdx int, cur float32)
	rec = func(slotIdx int, cur float32) {
		if cur > best {
			best = cur
		}
		if slotIdx == len(slotPositions) {
			return
		}
		rec(slotIdx+1, cur) // leave this slot unfilled
		for i, id := range ids {
			if used[i] || !flexEligible(slotPositions[slotIdx], posOf[id]) {
				continue
			}
			used[i] = true
			rec(slotIdx+1, cur+points[id])
			used[i] = false
		}
	}
	rec(0, 0)
	return best
}
