package sleeper

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/extensions/sleeper/sleepergen"
)

func digestCtx() context.Context {
	return sdk.WithCallInfo(context.Background(), sdk.CallInfo{ChatID: "ext:sleeper:L1:2:digest"})
}

func TestProjectionsHidden(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"ext:sleeper:L1:2:digest":     true,
		"ext:sleeper:L1:2:lineup":     false,
		"ext:sleeper:L1:2:retro":      false,
		"ext:github:acme-1:digest":    false,
		"ext:sleeper:L1:season-notes": false,
	}
	for id, want := range cases {
		ctx := sdk.WithCallInfo(context.Background(), sdk.CallInfo{ChatID: id})
		if got := projectionsHidden(ctx); got != want {
			t.Errorf("projectionsHidden(%q) = %v, want %v", id, got, want)
		}
	}
	if projectionsHidden(context.Background()) {
		t.Error("projectionsHidden with no CallInfo = true, want false")
	}
}

// TestGetMatchupDigestPreviewHidesProjections: week 2 is live, so the digest gets week 1's results instead.
func TestGetMatchupDigestPreviewHidesProjections(t *testing.T) {
	e := testExtension(t)
	got, err := e.getMatchup(digestCtx(), matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "projection") {
		t.Errorf("digest matchup leaks a projection field: %s", raw)
	}
	leagueID, _ := e.resolveLeagueID("")
	week1, err := e.client.Matchups(context.Background(), leagueID, 1)
	if err != nil {
		t.Fatalf("week 1 matchups: %v", err)
	}
	for _, sd := range []side{got.Me, got.Opponent} {
		m, _ := findMatchup(week1, sd.RosterID)
		if len(sd.RecentScores) != 1 || sd.RecentScores[0].Week != 1 || sd.RecentScores[0].Points != m.Points {
			t.Errorf("roster %d recent_scores = %+v, want week 1 at %v", sd.RosterID, sd.RecentScores, m.Points)
		}
		if sd.RecentAvg != round2(m.Points) {
			t.Errorf("roster %d recent_avg = %v, want %v", sd.RosterID, sd.RecentAvg, round2(m.Points))
		}
	}
}

func TestAddWeekFormJoinsStartersAndResult(t *testing.T) {
	t.Parallel()
	one, two := 1, 2
	matchups := []sleepergen.Matchup{
		{RosterId: 1, MatchupId: &one, Points: 101.5},
		{RosterId: 2, MatchupId: &one, Points: 99},
		{RosterId: 3, MatchupId: &two, Points: 80},
	}
	s := side{RosterID: 1, Starters: []lineupSlot{{PlayerID: "a"}, {PlayerID: "b"}}}
	addWeekForm(&s, 3, matchups, map[string]sleepergen.StatMap{"a": {"pts_ppr": 20}})
	addWeekForm(&s, 2, matchups, map[string]sleepergen.StatMap{"a": {"pts_ppr": 11}})
	want := teamWeek{Week: 3, Points: 101.5, OppPoints: 99, Result: "W"}
	if len(s.RecentScores) != 2 || s.RecentScores[0] != want {
		t.Errorf("recent_scores = %+v, want 2 weeks starting %+v", s.RecentScores, want)
	}
	if got := avgPoints(s.Starters[0].Recent, func(p weekPts) float32 { return p.PtsPPR }); got != 15.5 {
		t.Errorf("starter a avg = %v, want 15.5", got)
	}
	if s.Starters[1].Recent != nil {
		t.Errorf("starter b recent = %+v, want none (no stats row)", s.Starters[1].Recent)
	}
}

func TestGetMatchupLineupKeepsProjections(t *testing.T) {
	e := testExtension(t)
	ctx := sdk.WithCallInfo(context.Background(), sdk.CallInfo{ChatID: "ext:sleeper:L1:2:lineup"})
	got, err := e.getMatchup(ctx, matchupArgs{Week: 2})
	if err != nil {
		t.Fatalf("getMatchup: %v", err)
	}
	if got.Me.BestByProjection == nil || got.Me.Starters[0].Projection == nil {
		t.Error("lineup matchup lost its projections")
	}
	if got.Me.RecentScores != nil {
		t.Errorf("recent_scores = %+v, want omitted outside the digest", got.Me.RecentScores)
	}
}

func TestDigestRefusesFreeAgents(t *testing.T) {
	e := testExtension(t)
	if _, err := e.getFreeAgents(digestCtx(), freeAgentsArgs{}); err == nil {
		t.Error("getFreeAgents in a digest chat succeeded, want refused")
	}
}
