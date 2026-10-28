package sleeper

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/fagerbergj/quack/extensions/sdk"
)

// leagueScoring is the owner's league's real offensive scoring_settings.
var leagueScoring = map[string]float32{"pass_int": -1, "pass_2pt": 2, "rec_td": 6, "rush_td": 6, "rec_2pt": 2, "rec": 1,
	"fum_lost": -2, "rush_2pt": 2, "fum_rec": 2, "pass_yd": 0.04, "pass_td": 4, "rush_yd": 0.1, "fum": 0, "rec_yd": 0.1}

// TestScoreLineMatchesLeaguePoints: real week-5 lines against the league's players_points; the same sum matched all
// 852 rostered offensive player-weeks of weeks 1-7.
func TestScoreLineMatchesLeaguePoints(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		line map[string]float64
		want float64
	}{
		{"Justin Jefferson", map[string]float64{"rec": 6, "rec_yd": 79, "rec_td": 1, "rush_yd": -11, "fum": 1, "bonus_rec_wr": 6}, 18.8},
		{"Matthew Stafford", map[string]float64{"pass_yd": 383, "pass_td": 3, "pass_att": 54, "pass_sack": 1}, 27.32},
	} {
		if got := scoreLine(c.line, leagueScoring); got < c.want-0.005 || got > c.want+0.005 {
			t.Errorf("%s = %.2f, want Sleeper's %.2f", c.name, got, c.want)
		}
	}
}

// levels puts each probability on level i of a question.
func levels(ps ...float64) map[string]float64 {
	m := map[string]float64{}
	for i, p := range ps {
		m[strconv.Itoa(i)] = p
	}
	return m
}

// receiverDist: 4-5 catches, 60-79 yards, a 30% TD chance, no fumble.
func receiverDist() []statDist {
	qs := statLines["receiver_line"]
	return []statDist{{qs[0], levels(0, 0, 1)}, {qs[1], levels(0, 0, 0, 1)}, {qs[2], levels(0.7, 0.3)}, {qs[3], levels(1)}}
}

func TestPointsGridRange(t *testing.T) {
	t.Parallel()
	grid := pointsGrid(receiverDist(), leagueScoring, "WR", 1)
	lo, med, hi := gridQuantile(grid, 0.1), gridQuantile(grid, 0.5), gridQuantile(grid, 0.9)
	// u=0.11: 4.22 rec + 6.22 from yards; u=0.51 adds no TD yet; u=0.91 is past the 70% no-TD mass.
	if lo != 10.4 || med != 12 || hi != 19.6 {
		t.Errorf("range = %v/%v/%v, want 10.4/12/19.6", lo, med, hi)
	}
	q := pointsGrid(receiverDist(), leagueScoring, "WR", playRate("Questionable"))
	// The sit-out mass shifts every level: u=0.91 reads the healthy line at (0.91-0.293)/0.707 = 0.87.
	if gridQuantile(q, 0.1) != 0 || gridQuantile(q, 0.9) != 19.5 {
		t.Errorf("Questionable range low %v high %v, want 0 (a 29%% sit-out) and 19.5", gridQuantile(q, 0.1), gridQuantile(q, 0.9))
	}
	if p := pOutscores(grid, grid); p != 0.5 {
		t.Errorf("P(a > a) = %v, want 0.5", p)
	}
	if p := pOutscores(grid, make([]float64, gridN)); p != 1 {
		t.Errorf("P(range > zero) = %v, want 1", p)
	}
}

func TestPointsAtNegativeStatRunsBackward(t *testing.T) {
	t.Parallel()
	qs := statLines["qb_line"]
	dists := []statDist{{qs[0], levels(0, 0, 0, 1)}, {qs[2], levels(0.5, 0.5)}}
	_, bad := pointsAt(dists, leagueScoring, "QB", 0.1)
	_, good := pointsAt(dists, leagueScoring, "QB", 0.9)
	if bad["pass_int"] != 1 || good["pass_int"] != 0 {
		t.Errorf("interceptions at u=0.1 %v, u=0.9 %v; want a bad game to carry the pick", bad["pass_int"], good["pass_int"])
	}
}

// predictDecider answers every question of a stat-line point with an even spread over its levels.
type predictDecider struct {
	mu      sync.Mutex
	reqs    []sdk.DecideRequest
	outcome string
}

func (d *predictDecider) decide(_ context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	probs := map[string]map[string]float64{}
	for _, q := range statLines[req.Point] {
		probs[q.stat] = map[string]float64{}
		for i := range q.bands {
			probs[q.stat][strconv.Itoa(i)] = 1 / float64(len(q.bands))
		}
	}
	return sdk.Decision{Probabilities: probs, Outcome: d.outcome}, nil
}

func predictExt(t *testing.T, outcome string) (*extension, *predictDecider) {
	t.Helper()
	d := &predictDecider{outcome: outcome}
	e, _ := newTestExtension(t, sdk.Host{Decide: d.decide}, config{DefaultLeague: testLeague})
	return e, d
}

func TestPredictRangesAndCompare(t *testing.T) {
	e, d := predictExt(t, "fallback")
	ctx := sdk.WithCallInfo(context.Background(), sdk.CallInfo{ChatID: "ext:sleeper:" + testLeague + ":2:lineup"})
	// Payne Durham (TE) and Xavier Hutchinson (WR) have week-1 lines; Trey Benson is on IR; DEN has no stat model.
	res, err := e.predict(ctx, predictArgs{Players: []predictTarget{{PlayerID: "10227", News: "TE1 while the starter sits"},
		{PlayerID: "10218"}, {PlayerID: "11589"}, {PlayerID: "DEN"}}, Compare: []comparePair{{A: "10227", B: "10218"}, {A: "10227", B: "DEN"}}})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	te, ir, def := res.Players[0], res.Players[2], res.Players[3]
	if te.Low == nil || !(*te.Low <= *te.Median && *te.Median <= *te.High && *te.High > 0) || te.Stats["rec_yd"][2] == 0 {
		t.Errorf("TE prediction = %+v, want an ordered nonzero range with per-stat ranges", te)
	}
	if ir.High == nil || *ir.High != 0 || def.Low != nil || def.Note == "" {
		t.Errorf("IR = %+v, DEF = %+v; want IR at 0 and DEF with a note instead of numbers", ir, def)
	}
	if len(d.reqs) != 2 || d.reqs[0].Point != "receiver_line" || d.reqs[0].ChatID != "ext:sleeper:"+testLeague+":2:lineup" {
		t.Fatalf("decide calls = %+v, want one receiver_line call per modeled player, attributed to the chat", d.reqs)
	}
	b, _ := json.Marshal(d.reqs[0].State)
	var st predictState
	if err := json.Unmarshal(b, &st); err != nil || st.Player.ID != "10227" || len(st.Recent) != 1 || st.News == "" || st.Week != 2 {
		t.Errorf("state = %s, want Durham with his week-1 line, the news and the join keys", b)
	}
	if len(res.Compare) != 1 || res.Compare[0].PAOutscores != 0.5 {
		t.Errorf("compare = %+v, want only the modeled pair, at 0.5 for two identical spreads", res.Compare)
	}
}

func TestPredictObserveModeGivesNoNumbers(t *testing.T) {
	e, d := predictExt(t, "observe")
	res, err := e.predict(context.Background(), predictArgs{Players: []predictTarget{{PlayerID: "10227"}}})
	if err != nil {
		t.Fatalf("predict: %v", err)
	}
	if p := res.Players[0]; p.Low != nil || p.Note == "" || len(d.reqs) != 1 {
		t.Errorf("observe-mode prediction = %+v after %d calls, want the call recorded but no numbers", p, len(d.reqs))
	}
}
