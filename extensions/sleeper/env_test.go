package sleeper

import (
	"context"
	"reflect"
	"testing"

	"github.com/fagerbergj/quack/extensions/sleeper/sleepergen"
)

func depthPlayer(id, name, team, pos, slot string, order int, injury string) sleepergen.Player {
	p := sleepergen.Player{PlayerId: id, FullName: &name, Team: &team, Position: &pos, DepthChartPosition: &slot, DepthChartOrder: &order}
	if injury != "" {
		p.InjuryStatus = &injury
	}
	return p
}

func TestEnvOfAheadAndTeamInjuries(t *testing.T) {
	t.Parallel()
	dump := map[string]sleepergen.Player{
		"rb1": depthPlayer("rb1", "Lead Back", "SEA", "RB", "RB", 1, "Questionable"),
		"rb2": depthPlayer("rb2", "Handcuff", "SEA", "RB", "RB", 2, ""),
		"qb1": depthPlayer("qb1", "Starter QB", "SEA", "QB", "QB", 1, "Out"),
		"wr4": depthPlayer("wr4", "Deep WR", "SEA", "WR", "LWR", 4, "IR"),
		"te1": depthPlayer("te1", "Tight End", "SEA", "TE", "TE", 1, ""),
		"oth": depthPlayer("oth", "Other QB", "DAL", "QB", "QB", 1, "Out"),
	}
	td := newTeamDepth(dump)
	got := td.envOf(dump["rb2"])
	want := playerEnv{NFLTeam: "SEA", Depth: "RB2", Ahead: []teammate{{Name: "Lead Back", Pos: "RB", Depth: "RB1", Injury: "Questionable"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("handcuff env = %+v, want %+v", got, want)
	}
	if a := td.envOf(dump["te1"]).Ahead; a != nil {
		t.Errorf("TE1 ahead = %+v, want none", a)
	}
	inj := td.injuries([]string{"SEA"})
	if len(inj) != 1 || len(inj["SEA"]) != 2 || inj["SEA"][0].Name != "Starter QB" || inj["SEA"][1].Name != "Lead Back" {
		t.Errorf("SEA injuries = %+v, want QB1 and RB1 only (WR4 is too deep, DAL not asked)", inj)
	}
	if td.injuries([]string{"NYJ"}) != nil {
		t.Error("injuries for a team with none, want nil")
	}
}

func TestLineupAvgZeroesByeAndRuledOut(t *testing.T) {
	t.Parallel()
	ten, eight, six, four := float32(10), float32(8), float32(6), float32(4)
	starters := []lineupSlot{
		{RecentAvg: &ten},
		{RecentAvg: &eight, gameInfo: gameInfo{Bye: true}},
		{RecentAvg: &six, playerEnv: playerEnv{Injury: "Out"}},
		{RecentAvg: &four, playerEnv: playerEnv{Injury: "Questionable"}},
		{RecentAvg: &six, playerEnv: playerEnv{Injury: "Sus"}},
		{RecentAvg: &six, playerEnv: playerEnv{Injury: "IR"}},
		{RecentAvg: &six, playerEnv: playerEnv{Injury: "Doubtful"}},
		{},
	}
	if got := lineupAvg(starters); got == nil || *got != 14 {
		t.Errorf("lineupAvg = %v, want 14 (bye, Out, Sus, IR and Doubtful count 0, Questionable counts)", got)
	}
	if lineupAvg([]lineupSlot{{}}) != nil {
		t.Error("lineupAvg with no recent averages, want nil")
	}
}

func TestGetMatchupEnvOnlyForUnplayedWeek(t *testing.T) {
	e := testExtension(t)
	past, err := e.getMatchup(context.Background(), matchupArgs{Week: 1})
	if err != nil {
		t.Fatalf("getMatchup week 1: %v", err)
	}
	for _, s := range past.Me.Starters {
		if s.playerEnv.NFLTeam != "" || s.Ahead != nil {
			t.Errorf("week 1 starter %s carries today's situation %+v, want none for a finished week", s.Name, s.playerEnv)
		}
	}
	live, err := e.getMatchup(digestCtx(), matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup week 2: %v", err)
	}
	if live.Me.Starters[0].NFLTeam == "" {
		t.Errorf("week 2 starter %+v has no nfl_team", live.Me.Starters[0])
	}
}
