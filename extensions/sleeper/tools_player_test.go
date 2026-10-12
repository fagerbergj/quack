package sleeper

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

func TestGetPlayerByID(t *testing.T) {
	e := testExtension(t)
	got, err := e.getPlayer(context.Background(), playerArgs{PlayerID: "6797", TZ: "America/Chicago"})
	if err != nil {
		t.Fatalf("getPlayer: %v", err)
	}
	if got.Name != "Justin Herbert" {
		t.Errorf("name = %q, want Justin Herbert", got.Name)
	}
	if got.Position != "QB" || got.Team != "LAC" {
		t.Errorf("position/team = %q/%q, want QB/LAC", got.Position, got.Team)
	}
	if got.FetchedAt == "" {
		t.Error("expected fetched_at to be set")
	}
	g := got.Game
	if g == nil || g.GameID != "202610229" || g.NFLOpponent != "LV" || !g.IsHome || g.Roof != "outdoor" || g.Venue != "SoFi Stadium" ||
		g.Kickoff != "2026-09-20T20:05:00Z" || g.KickoffLocal != "Sun Sep 20 3:05 PM CDT" {
		t.Errorf("game = %+v, want week-2 LV @ LAC at SoFi, 3:05 PM CDT", g)
	}
}

func TestGetPlayerByName(t *testing.T) {
	e := testExtension(t)
	got, err := e.getPlayer(context.Background(), playerArgs{Name: "Buccaneers"})
	if err != nil {
		t.Fatalf("getPlayer: %v", err)
	}
	if got.PlayerID != "TB" {
		t.Errorf("player_id = %q, want TB", got.PlayerID)
	}
}

func TestGetPlayerRequiresIDOrName(t *testing.T) {
	e := testExtension(t)
	if _, err := e.getPlayer(context.Background(), playerArgs{}); err == nil {
		t.Fatal("expected an error when neither player_id nor name is given")
	}
}

// TestGetPlayerGameLog: the recorded live fixture is keyed by week with nulls for unplayed weeks; week 2
// is the fixture league's current (in-progress) week, so only week 1 must survive.
func TestGetPlayerGameLog(t *testing.T) {
	e := testExtension(t)
	got, err := e.getPlayer(context.Background(), playerArgs{PlayerID: "6797"})
	if err != nil {
		t.Fatalf("getPlayer: %v", err)
	}
	if len(got.GameLog) != 1 {
		t.Fatalf("game_log = %+v, want exactly week 1 (week 2 is the current week)", got.GameLog)
	}
	w := got.GameLog[0]
	if w.Week != 1 || w.Opp != "ARI" {
		t.Errorf("week/opp = %d/%q, want 1/ARI", w.Week, w.Opp)
	}
	if w.SnapPct == nil || *w.SnapPct != 100 {
		t.Errorf("snap_pct = %v, want 100 (55/55)", w.SnapPct)
	}
	if w.Carries != 5 || w.PtsPPR != 14.26 {
		t.Errorf("carries/pts_ppr = %v/%v, want 5/14.26", w.Carries, w.PtsPPR)
	}
}

func TestBuildGameLog(t *testing.T) {
	week := func(w int, stats map[string]float32) sleepergen.PlayerStatEntry {
		return sleepergen.PlayerStatEntry{PlayerId: "p", Season: "2026", SeasonType: "regular", Week: &w, Stats: &stats}
	}
	t.Run("excludes the current and future weeks", func(t *testing.T) {
		entries := []sleepergen.PlayerStatEntry{week(1, nil), week(2, nil), week(3, nil)}
		got := buildGameLog(entries, 2)
		if len(got) != 1 || got[0].Week != 1 {
			t.Errorf("buildGameLog = %+v, want only week 1 (currentWeek=2)", got)
		}
	})
	t.Run("caps at maxGameLogWeeks, newest first", func(t *testing.T) {
		var entries []sleepergen.PlayerStatEntry
		for w := 1; w <= 7; w++ {
			entries = append(entries, week(w, nil))
		}
		got := buildGameLog(entries, 8)
		if len(got) != maxGameLogWeeks {
			t.Fatalf("len = %d, want %d", len(got), maxGameLogWeeks)
		}
		for i, wantWeek := 0, 7; i < len(got); i, wantWeek = i+1, wantWeek-1 {
			if got[i].Week != wantWeek {
				t.Errorf("got[%d].Week = %d, want %d (newest first)", i, got[i].Week, wantWeek)
			}
		}
	})
	t.Run("a week with no stats row is dropped", func(t *testing.T) {
		entries := []sleepergen.PlayerStatEntry{{PlayerId: "p", Season: "2026", SeasonType: "regular", Week: nil}}
		if got := buildGameLog(entries, 5); len(got) != 0 {
			t.Errorf("buildGameLog = %+v, want none (nil week)", got)
		}
	})
}

func TestSnapPct(t *testing.T) {
	cases := []struct {
		name  string
		stats map[string]float32
		want  *int
	}{
		{"normal share", map[string]float32{"off_snp": 65, "tm_off_snp": 70}, intPtr(93)},
		{"missing tm_off_snp", map[string]float32{"off_snp": 65}, nil},
		{"zero tm_off_snp", map[string]float32{"off_snp": 0, "tm_off_snp": 0}, nil},
		{"full share rounds to 100", map[string]float32{"off_snp": 70, "tm_off_snp": 70}, intPtr(100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := snapPct(tc.stats)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("snapPct(%v) = %v, want %v", tc.stats, got, tc.want)
			}
		})
	}
}

func intPtr(i int) *int { return &i }

// TestPlayerName pins the one label both the tools and the UI's recent moves use.
func TestPlayerName(t *testing.T) {
	s := func(v string) *string { return &v }
	dump := map[string]sleepergen.Player{
		"full":  {PlayerId: "full", FullName: s("Josh Allen"), FirstName: s("x")},
		"first": {PlayerId: "first", FirstName: s("Josh")},
		"def":   {PlayerId: "def", Position: s("DEF"), Team: s("BAL")},
		"bare":  {PlayerId: "bare"},
	}
	for id, want := range map[string]string{"full": "Josh Allen", "first": "Josh", "def": "BAL", "bare": "bare", "missing": "missing"} {
		if got := playerName(dump, id); got != want {
			t.Errorf("playerName(%q) = %q, want %q", id, got, want)
		}
	}
}
