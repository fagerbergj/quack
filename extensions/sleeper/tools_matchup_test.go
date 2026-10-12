package sleeper

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

func TestGetMatchup(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Week != 2 {
		t.Errorf("week = %d, want 2", got.Week)
	}
	if got.Me.RosterID == 0 {
		t.Error("expected my roster to resolve")
	}
	if len(got.Me.Starters) != 9 {
		t.Errorf("my starters = %d, want 9", len(got.Me.Starters))
	}
	if got.Opponent.RosterID == 0 {
		t.Error("expected an opponent to resolve from matchup_id pairing")
	}
	if got.MatchupID == 0 {
		t.Error("expected a matchup_id")
	}
	for _, sd := range []side{got.Me, got.Opponent} {
		for _, p := range sd.Starters {
			if p.Game == nil || p.Game.NFLOpponent == "" || p.Game.Kickoff == "" || p.Game.KickoffLocal == "" {
				t.Errorf("roster %d starter %s: game %+v (reason %q), want a joined game", sd.RosterID, p.Name, p.Game, p.NoGameReason)
			}
		}
	}
}

func TestGetMatchupPastWeekLabeledAndJoined(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 1, TZ: "nowhere"})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.WeekNote == "" || got.CurrentWeek != 2 || got.TZNote == "" {
		t.Errorf("week 1: week_note=%q current_week=%d tz_note=%q, want not-current label and tz note", got.WeekNote, got.CurrentWeek, got.TZNote)
	}
	// Players' teams are today's, so a past week must not be joined to games.
	for _, b := range got.Me.Bench {
		if b.Game != nil || b.NoGameReason != "games are joined only for the current NFL week" {
			t.Errorf("bench %s: game %+v reason %q, want no join", b.Name, b.Game, b.NoGameReason)
		}
	}
}

func TestGetMatchupExplicitRoster(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 2, RosterID: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Me.RosterID != 2 {
		t.Errorf("roster_id = %d, want 2", got.Me.RosterID)
	}
}

// TestGetMatchupCurrentWeekOmitsRetroFields: week 2 is the fixture's live
// current week, so bench/best_points/free_agent_hits must stay absent.
func TestGetMatchupCurrentWeekOmitsRetroFields(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Me.Bench != nil {
		t.Errorf("bench = %v, want omitted for the current week", got.Me.Bench)
	}
	if got.Me.BestPoints != 0 || got.Me.LeftOnBench != 0 {
		t.Errorf("best_points/left_on_bench = %v/%v, want omitted for the current week", got.Me.BestPoints, got.Me.LeftOnBench)
	}
	if got.Me.FreeAgentHits != nil {
		t.Errorf("free_agent_hits = %v, want omitted for the current week", got.Me.FreeAgentHits)
	}
}

// TestGetMatchupPastWeekRetroFields: week 1 is complete (state.Week is 2),
// so best/left are pinned against an independently hand-solved lineup.
func TestGetMatchupPastWeekRetroFields(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 1})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if len(got.Me.Bench) == 0 {
		t.Fatal("expected bench players for a completed week")
	}
	if got.Me.BestPoints != 132.06 {
		t.Errorf("best_points = %v, want 132.06", got.Me.BestPoints)
	}
	if got.Me.LeftOnBench != 31.4 {
		t.Errorf("left_on_bench = %v, want 31.4", got.Me.LeftOnBench)
	}
	if len(got.Me.FreeAgentHits) == 0 {
		t.Fatal("expected at least one free-agent hit")
	}
	hit := got.Me.FreeAgentHits[0]
	if hit.Name != "Payne Durham" || hit.Pos != "TE" {
		t.Errorf("top hit = %+v, want Payne Durham (TE)", hit)
	}
	if hit.Over.Name != "Colston Loveland" {
		t.Errorf("hit.over = %+v, want to beat Colston Loveland's started TE", hit.Over)
	}
}

func TestWeekIsComplete(t *testing.T) {
	cases := []struct {
		name   string
		week   int
		status string
		want   bool
	}{
		{"past week, in-season", 1, "in_season", true},
		{"current week, in-season", 2, "in_season", false},
		{"future week, in-season", 3, "in_season", false},
		{"current week, season complete", 2, "complete", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			league := &sleepergen.League{Status: tc.status}
			state := &sleepergen.NflState{Week: 2}
			if got := weekIsComplete(tc.week, league, state); got != tc.want {
				t.Errorf("weekIsComplete(%d, status=%q) = %v, want %v", tc.week, tc.status, got, tc.want)
			}
		})
	}
}

func TestBenchPlayers(t *testing.T) {
	dump := playerDumpAt("QB1", "QB", "RB1", "RB", "RB2", "RB")
	proj := map[string]sleepergen.StatMap{"RB2": {"pts_ppr": 9.5}}
	m := sleepergen.Matchup{
		Players:       []string{"QB1", "RB1", "RB2", "0"},
		Starters:      []string{"QB1", "RB1"},
		PlayersPoints: map[string]float32{"QB1": 10, "RB1": 8, "RB2": 7},
	}
	bench := benchPlayers(m, dump, proj, slate{})
	if len(bench) != 1 {
		t.Fatalf("bench = %+v, want exactly RB2 (starters and the \"0\" placeholder excluded)", bench)
	}
	if bench[0].Pos != "RB" || bench[0].Points != 7 || bench[0].Projection != 9.5 {
		t.Errorf("bench[0] = %+v, want RB2's pos/points/projection", bench[0])
	}
}

// TestAddRetroFieldsFlexPromotesFromBench: a bench RB (RB2) outscores the
// started FLEX (WR2), so best_points must reflect the swap.
func TestAddRetroFieldsFlexPromotesFromBench(t *testing.T) {
	dump := playerDumpAt("QB1", "QB", "RB1", "RB", "WR1", "WR", "WR2", "WR", "RB2", "RB")
	league := &sleepergen.League{RosterPositions: []string{"QB", "RB", "WR", "FLEX", "BN"}}
	m := sleepergen.Matchup{
		Points:        30, // QB1(10) + RB1(9) + WR1(6) + WR2(5), the started FLEX
		Players:       []string{"QB1", "RB1", "WR1", "WR2", "RB2"},
		Starters:      []string{"QB1", "RB1", "WR1", "WR2"},
		PlayersPoints: map[string]float32{"QB1": 10, "RB1": 9, "WR1": 6, "WR2": 5, "RB2": 7},
	}
	me := &side{Starters: []lineupSlot{
		{Slot: "QB", PlayerID: "QB1", Points: 10},
		{Slot: "RB", PlayerID: "RB1", Points: 9},
		{Slot: "WR", PlayerID: "WR1", Points: 6},
		{Slot: "FLEX", PlayerID: "WR2", Points: 5},
	}}
	proj := map[string]sleepergen.StatMap{}
	stats := map[string]sleepergen.StatMap{}
	addRetroFields(me, league, m, []sleepergen.Matchup{m}, dump, proj, stats, slate{})
	if me.BestPoints != 32 {
		t.Errorf("best_points = %v, want 32 (bench RB2 promoted into FLEX over started WR2)", me.BestPoints)
	}
	if me.LeftOnBench != 2 {
		t.Errorf("left_on_bench = %v, want 2 (32 best - 30 started)", me.LeftOnBench)
	}
	if len(me.Swaps) != 1 {
		t.Fatalf("swaps = %+v, want exactly one (bench RB2 in for started WR2)", me.Swaps)
	}
	sw := me.Swaps[0]
	if sw.In.Pos != "RB" || sw.In.Points != 7 || sw.Out.Slot != "FLEX" || sw.Out.Points != 5 || sw.Swing != 2 {
		t.Errorf("swap = %+v, want in RB@7 out FLEX@5 swing 2", sw)
	}
}

// TestBuildSwapsSameSetSwappedOrderIsZeroSwaps: bestLineup fills strict
// slots by points, so a slot-by-slot comparison of a reordered RB1/RB2 would wrongly report two misses.
func TestBuildSwapsSameSetSwappedOrderIsZeroSwaps(t *testing.T) {
	dump := playerDumpAt("QB1", "QB", "RB1", "RB", "RB2", "RB")
	league := &sleepergen.League{RosterPositions: []string{"QB", "RB", "RB", "BN"}}
	// Started RB1 (lower scorer) then RB2 (higher scorer); bestLineup's
	// top-N-per-slot fill assigns the higher scorer to the first RB slot.
	m := sleepergen.Matchup{
		Points:        29,
		Players:       []string{"QB1", "RB1", "RB2"},
		Starters:      []string{"QB1", "RB1", "RB2"},
		PlayersPoints: map[string]float32{"QB1": 10, "RB1": 8, "RB2": 11},
	}
	me := &side{Starters: []lineupSlot{
		{Slot: "QB", PlayerID: "QB1", Points: 10},
		{Slot: "RB", PlayerID: "RB1", Points: 8},
		{Slot: "RB", PlayerID: "RB2", Points: 11},
	}}
	assignment := bestLineup(league.RosterPositions, m.Players, m.PlayersPoints, dump)
	if assignment.bySlot[1] != "RB2" || assignment.bySlot[2] != "RB1" {
		t.Fatalf("assignment = %+v, want RB2 then RB1 (solver fills by points, not start order)", assignment.bySlot)
	}
	if swaps := buildSwaps(me.Starters, assignment, m.PlayersPoints, dump); len(swaps) != 0 {
		t.Errorf("swaps = %+v, want none - same two players started, just in swapped slots", swaps)
	}
}

func TestFreeAgentHits(t *testing.T) {
	dump := playerDumpAt(
		"QB1", "QB", "WR1", "WR", "TE1", "TE",
		"AgentTE", "TE", "AgentWRLow", "WR", "AgentOL", "OL",
	)
	starters := []lineupSlot{
		{Slot: "QB", PlayerID: "QB1", Name: "QB1", Points: 20},
		{Slot: "WR", PlayerID: "WR1", Name: "WR1", Points: 10},
		{Slot: "TE", PlayerID: "TE1", Name: "TE1", Points: 2},
	}
	rosteredMatchup := sleepergen.Matchup{Players: []string{"QB1", "WR1", "TE1"}}
	stats := map[string]sleepergen.StatMap{
		"AgentTE":    {"pts_ppr": 15}, // unrostered TE beats TE1's 2 -> a hit
		"AgentWRLow": {"pts_ppr": 4},  // unrostered WR but below WR1's 10 -> not a hit
		"AgentOL":    {"pts_ppr": 99}, // huge score, but OL fills no roster slot -> not a hit
		"QB1":        {"pts_ppr": 20}, // rostered -> never a candidate
	}
	hits := freeAgentHits(starters, []sleepergen.Matchup{rosteredMatchup}, dump, stats)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly the AgentTE hit", hits)
	}
	h := hits[0]
	if h.Name != "AgentTE" || h.Pos != "TE" || h.Points != 15 {
		t.Errorf("hit = %+v, want AgentTE/TE/15", h)
	}
	if h.Over.Name != "TE1" || h.Over.Points != 2 {
		t.Errorf("hit.over = %+v, want to beat TE1's 2", h.Over)
	}
}

func TestFreeAgentHitsCapAndOrder(t *testing.T) {
	names := []string{"H1", "H2", "H3", "H4", "H5", "H6"}
	pointsByName := map[string]float32{"H1": 30, "H2": 25, "H3": 20, "H4": 15, "H5": 10, "H6": 5}
	var dumpArgs []string
	stats := map[string]sleepergen.StatMap{}
	for _, n := range names {
		dumpArgs = append(dumpArgs, n, "WR")
		stats[n] = sleepergen.StatMap{"pts_ppr": pointsByName[n]}
	}
	dump := playerDumpAt(dumpArgs...)
	starters := []lineupSlot{{Slot: "WR", PlayerID: "W", Name: "W", Points: 1}}
	hits := freeAgentHits(starters, nil, dump, stats)
	if len(hits) != maxFreeAgentHits {
		t.Fatalf("hits = %d, want capped at %d", len(hits), maxFreeAgentHits)
	}
	for i, want := range []string{"H1", "H2", "H3", "H4", "H5"} {
		if hits[i].Name != want {
			t.Errorf("hits[%d].Name = %q, want %q (descending swing)", i, hits[i].Name, want)
		}
	}
}

func TestNumberedSlots(t *testing.T) {
	got := numberedSlots([]string{"QB", "RB", "RB", "WR", "WR", "TE", "FLEX", "K", "DEF", "BN", "BN"})
	want := []string{"QB", "RB1", "RB2", "WR1", "WR2", "TE", "FLEX", "K", "DEF"}
	if len(got) != len(want) {
		t.Fatalf("numberedSlots = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("numberedSlots[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func withInjury(dump map[string]sleepergen.Player, id, status string) map[string]sleepergen.Player {
	out := make(map[string]sleepergen.Player, len(dump))
	for k, v := range dump {
		out[k] = v
	}
	p := out[id]
	p.InjuryStatus = &status
	out[id] = p
	return out
}

// TestBestByProjectionSlots: a benched WR
// (Williams) projected higher than the started WR2 (Vele) must win WR2, not just FLEX.
func TestBestByProjectionSlots(t *testing.T) {
	dump := playerDumpAt("QB1", "QB", "WRSTART1", "WR", "VELE", "WR", "WILLIAMS", "WR", "FLEXRB", "RB")
	proj := map[string]sleepergen.StatMap{
		"QB1": {"pts_ppr": 20}, "WRSTART1": {"pts_ppr": 15},
		"VELE": {"pts_ppr": 10.12}, "WILLIAMS": {"pts_ppr": 12.64}, "FLEXRB": {"pts_ppr": 5},
	}
	roster := []string{"QB", "WR", "WR", "FLEX", "BN"}
	players := []string{"QB1", "WRSTART1", "VELE", "WILLIAMS", "FLEXRB"}
	slotNames := func(got []bestByProjectionSlot) map[string]string {
		out := map[string]string{}
		for _, s := range got {
			out[s.Slot] = s.Name
		}
		return out
	}

	t.Run("benched higher-projection WR wins WR2 over the started FLEX comparison", func(t *testing.T) {
		got := bestByProjectionSlots(roster, players, proj, dump)
		bySlot := slotNames(got)
		if bySlot["WR2"] != "WILLIAMS" {
			t.Errorf("WR2 = %q, want WILLIAMS (12.64 beats Vele's 10.12)", bySlot["WR2"])
		}
		if bySlot["FLEX"] != "VELE" {
			t.Errorf("FLEX = %q, want VELE", bySlot["FLEX"])
		}
	})

	t.Run("Out player excluded from candidates entirely", func(t *testing.T) {
		injured := withInjury(dump, "WILLIAMS", "Out")
		got := bestByProjectionSlots(roster, players, proj, injured)
		bySlot := slotNames(got)
		for _, s := range got {
			if s.Name == "WILLIAMS" {
				t.Fatalf("Out player WILLIAMS must never be assigned a slot, got %+v", got)
			}
		}
		if bySlot["WR2"] != "VELE" {
			t.Errorf("WR2 = %q, want VELE once WILLIAMS is excluded", bySlot["WR2"])
		}
		if bySlot["FLEX"] != "FLEXRB" {
			t.Errorf("FLEX = %q, want FLEXRB (WR leftover empty once WILLIAMS is excluded)", bySlot["FLEX"])
		}
	})

	t.Run("Doubtful and IR are excluded the same as Out", func(t *testing.T) {
		for _, status := range []string{"Doubtful", "IR"} {
			injured := withInjury(dump, "WILLIAMS", status)
			got := bestByProjectionSlots(roster, players, proj, injured)
			for _, s := range got {
				if s.Name == "WILLIAMS" {
					t.Errorf("status %q: WILLIAMS must be excluded, got %+v", status, got)
				}
			}
		}
	})

	t.Run("Questionable player stays a candidate", func(t *testing.T) {
		questionable := withInjury(dump, "WILLIAMS", "Questionable")
		got := bestByProjectionSlots(roster, players, proj, questionable)
		if slotNames(got)["WR2"] != "WILLIAMS" {
			t.Errorf("WR2 = %q, want WILLIAMS (Questionable stays a candidate)", slotNames(got)["WR2"])
		}
	})

	t.Run("FLEX picks the best remaining regardless of position", func(t *testing.T) {
		bumped := map[string]sleepergen.StatMap{}
		for k, v := range proj {
			bumped[k] = v
		}
		bumped["FLEXRB"] = sleepergen.StatMap{"pts_ppr": 11} // now beats Vele's 10.12 leftover
		got := bestByProjectionSlots(roster, players, bumped, dump)
		if slotNames(got)["FLEX"] != "FLEXRB" {
			t.Errorf("FLEX = %q, want FLEXRB (11 beats Vele's 10.12)", slotNames(got)["FLEX"])
		}
	})
}

func TestExcludedInjuryStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"Out", true}, {"IR", true}, {"Doubtful", true},
		{"Questionable", false}, {"", false},
	}
	for _, tc := range cases {
		status := tc.status
		if got := excludedInjuryStatus(&status); got != tc.want {
			t.Errorf("excludedInjuryStatus(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
	if excludedInjuryStatus(nil) {
		t.Error("a nil injury status must not be excluded")
	}
}

// TestGetMatchupCurrentWeekBestByProjection: the current week must offer
// best_by_projection instead of best_lineup, never both.
func TestGetMatchupCurrentWeekBestByProjection(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if len(got.Me.BestByProjection) == 0 {
		t.Fatal("expected best_by_projection for the current week")
	}
	if got.Me.BestLineup != nil {
		t.Errorf("best_lineup = %v, want omitted for the current week", got.Me.BestLineup)
	}
	slots := map[string]bool{}
	for _, s := range got.Me.BestByProjection {
		slots[s.Slot] = true
	}
	for _, want := range []string{"QB", "RB1", "RB2", "WR1", "WR2", "TE", "FLEX", "K", "DEF"} {
		if !slots[want] {
			t.Errorf("best_by_projection missing slot %q: %+v", want, got.Me.BestByProjection)
		}
	}
}

// TestGetMatchupPastWeekBestLineup: RB1/RB2 go to Hubbard/Irving (bench
// RBs that outscored Dowdle) and FLEX goes to CMC, never best_by_projection.
func TestGetMatchupPastWeekBestLineup(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(context.Background(), matchupArgs{Week: 1})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Me.BestByProjection != nil {
		t.Errorf("best_by_projection = %v, want omitted for a past week", got.Me.BestByProjection)
	}
	bySlot := map[string]string{}
	for _, s := range got.Me.BestLineup {
		bySlot[s.Slot] = s.Name
	}
	if bySlot["FLEX"] != "Christian McCaffrey" {
		t.Errorf("best_lineup FLEX = %q, want Christian McCaffrey", bySlot["FLEX"])
	}
	rbSlots := []string{bySlot["RB1"], bySlot["RB2"]}
	if !((rbSlots[0] == "Chuba Hubbard" && rbSlots[1] == "Bucky Irving") || (rbSlots[0] == "Bucky Irving" && rbSlots[1] == "Chuba Hubbard")) {
		t.Errorf("best_lineup RB1/RB2 = %v, want Hubbard and Irving in some order", rbSlots)
	}
}
