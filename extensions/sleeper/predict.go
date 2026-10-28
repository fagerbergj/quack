// sleeper_predict: the decision model's per-stat distributions, scored with the league's own settings.
package sleeper

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/extensions/sleeper/sleepergen"
)

// band is one score level of a stat question: values in [lo, hi), or exactly lo when hi == lo.
type band struct {
	label  string
	lo, hi float64
}

type statQuestion struct {
	stat, noun string
	bands      []band
}

// span makes "a-b noun" levels from edges; the top level reads "a+ noun" and spreads up to the last edge.
func span(stat, noun string, edges ...float64) statQuestion {
	var out []band
	for i := 0; i+1 < len(edges); i++ {
		label := fmt.Sprintf("%g-%g %s", edges[i], edges[i+1]-1, noun)
		if i+2 == len(edges) {
			label = fmt.Sprintf("%g+ %s", edges[i], noun)
		}
		out = append(out, band{label, edges[i], edges[i+1]})
	}
	return statQuestion{stat, noun, out}
}

// count makes "0 noun" ... "top+ noun" point levels.
func count(stat, noun string, top int) statQuestion {
	var out []band
	for i := 0; i <= top; i++ {
		label := fmt.Sprintf("%d %s", i, noun)
		if i == top {
			label = fmt.Sprintf("%d+ %s", i, noun)
		}
		out = append(out, band{label, float64(i), float64(i)})
	}
	return statQuestion{stat, noun, out}
}

var (
	fumLost    = count("fum_lost", "fumbles lost", 1)
	rushTD     = count("rush_td", "rushing TDs", 2)
	recTD      = count("rec_td", "receiving TDs", 2)
	receptions = span("rec", "receptions", 0, 2, 4, 6, 8, 10, 14)

	// statLines are the stats each point asks, one request per player; K and DEF score buckets these can't model.
	statLines = map[string][]statQuestion{
		"qb_line": {span("pass_yd", "passing yards", 0, 150, 200, 250, 300, 350, 450), count("pass_td", "passing TDs", 4),
			count("pass_int", "interceptions", 2), span("rush_yd", "rushing yards", 0, 10, 20, 40, 60, 90), rushTD, fumLost},
		"rb_line": {span("rush_yd", "rushing yards", 0, 20, 40, 60, 80, 100, 130, 180), rushTD, receptions,
			span("rec_yd", "receiving yards", 0, 10, 25, 40, 60, 90), recTD, fumLost},
		"receiver_line": {receptions, span("rec_yd", "receiving yards", 0, 20, 40, 60, 80, 100, 130, 190), recTD, fumLost},
	}
	linePrimary = map[string]string{"qb_line": "pass_yd", "rb_line": "rush_yd", "receiver_line": "rec_yd"}
	lineFor     = map[string]string{"QB": "qb_line", "RB": "rb_line", "WR": "receiver_line", "TE": "receiver_line"}
)

var predictPoints = func() []sdk.DecisionPoint {
	var out []sdk.DecisionPoint
	for _, name := range []string{"qb_line", "rb_line", "receiver_line"} {
		qs := map[string]sdk.DecisionQuestion{}
		for _, q := range statLines[name] {
			labels := make([]string, len(q.bands))
			for i, b := range q.bands {
				labels[i] = b.label
			}
			qs[q.stat] = sdk.DecisionQuestion{Type: "score", Criteria: labels,
				Instructions: "If this player plays this week's game, how many " + q.noun + " will he have?"}
		}
		out = append(out, sdk.DecisionPoint{Name: name, Description: "one player's stat line for this week, per stat, for sleeper_predict's ranges",
			Questions: qs, Primary: linePrimary[name], Modes: []string{"observe", "decide"}})
	}
	return out
}()

// quantile reads the q-th quantile of one stat from its level probabilities, uniform within a span level.
func quantile(probs map[string]float64, bands []band, q float64) float64 {
	total := 0.0
	for i := range bands {
		total += probs[strconv.Itoa(i)]
	}
	if total == 0 {
		return bands[0].lo
	}
	cum := 0.0
	for i, b := range bands {
		p := probs[strconv.Itoa(i)] / total
		if p > 0 && cum+p >= q {
			return b.lo + (q-cum)/p*(b.hi-b.lo)
		}
		cum += p
	}
	return bands[len(bands)-1].lo
}

// scoreLine is the league's linear scoring over a stat line; it reproduces Sleeper's players_points for skill players.
func scoreLine(line map[string]float64, scoring map[string]float32) float64 {
	pts := 0.0
	for k, v := range line {
		pts += float64(scoring[k]) * v
	}
	return pts
}

type statDist struct {
	q     statQuestion
	probs map[string]float64
}

// pointsAt scores every stat at quantile u, as if one player's stats rise and fall together (a good game is
// good throughout); a stat the league scores negatively takes 1-u.
func pointsAt(dists []statDist, scoring map[string]float32, pos string, u float64) (float64, map[string]float64) {
	line := map[string]float64{}
	for _, d := range dists {
		q := u
		if scoring[d.q.stat] < 0 {
			q = 1 - u
		}
		line[d.q.stat] = quantile(d.probs, d.q.bands, q)
	}
	line["bonus_rec_"+strings.ToLower(pos)] = line["rec"]
	return scoreLine(line, scoring), line
}

// playRate is the chance a designated player suits up: Questionable skill players played 70.7% (Footballguys,
// 2017-2024, in sleeper:start-sit); Out/IR/Doubtful/Sus count 0, as lineup_avg does.
func playRate(injury string) float64 {
	switch {
	case excludedInjuryStatus(&injury):
		return 0
	case injury == "Questionable":
		return 0.707
	}
	return 1
}

const gridN = 50

// pointsGrid is the player's point quantiles at gridN evenly spaced levels, a sit-out mixed in as 0.
func pointsGrid(dists []statDist, scoring map[string]float32, pos string, play float64) []float64 {
	grid := make([]float64, gridN)
	for i := range grid {
		u := (float64(i) + 0.5) / gridN
		if u >= 1-play {
			grid[i], _ = pointsAt(dists, scoring, pos, (u-(1-play))/play)
		}
	}
	return grid
}

func gridQuantile(grid []float64, q float64) float32 {
	return round1(grid[min(int(q*gridN), gridN-1)])
}

// pOutscores is P(a > b) for independent players, ties split.
func pOutscores(a, b []float64) float32 {
	wins := 0.0
	for _, x := range a {
		for _, y := range b {
			switch {
			case x > y:
				wins++
			case x == y:
				wins += 0.5
			}
		}
	}
	return float32(math.Round(wins/float64(len(a)*len(b))*100) / 100)
}

func round1(v float64) float32 { return float32(math.Round(v*10) / 10) }

type predictArgs struct {
	LeagueID string          `json:"league_id,omitempty"`
	Players  []predictTarget `json:"players"`
	Compare  []comparePair   `json:"compare,omitempty"`
}

type predictTarget struct {
	PlayerID string `json:"player_id"`
	News     string `json:"news,omitempty"`
}

type comparePair struct {
	A string `json:"a"`
	B string `json:"b"`
}

type prediction struct {
	PlayerID string                `json:"player_id"`
	Name     string                `json:"name"`
	Pos      string                `json:"pos"`
	Low      *float32              `json:"low,omitempty"`
	Median   *float32              `json:"median,omitempty"`
	High     *float32              `json:"high,omitempty"`
	Stats    map[string][3]float32 `json:"stats,omitempty"`
	Note     string                `json:"note,omitempty"`
	grid     []float64
}

type comparison struct {
	A           string  `json:"a"`
	B           string  `json:"b"`
	PAOutscores float32 `json:"p_a_outscores_b"`
}

type predictResult struct {
	callContext
	Players []prediction `json:"players"`
	Compare []comparison `json:"compare,omitempty"`
}

// predictState is what the decision model reads for one player: facts only, plus the agent's news notes.
type predictState struct {
	decisionKeys
	Player       predictRow `json:"player"`
	TeamInjuries []teammate `json:"team_injuries,omitempty"`
	Recent       []statWeek `json:"recent,omitempty"`
	OppAllowed   *allowed   `json:"opp_allowed,omitempty"`
	News         string     `json:"news,omitempty"`
}

type predictRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Pos  string `json:"pos"`
	Opp  string `json:"opp,omitempty"`
	Home bool   `json:"home"`
	playerEnv
}

type statWeek struct {
	Week    int                `json:"week"`
	SnapPct *float32           `json:"snap_pct,omitempty"`
	Stats   map[string]float32 `json:"stats"`
}

// allowed is the opponent's average PPR points given up per game to the player's position.
type allowed struct {
	Pos    string  `json:"pos"`
	Games  int     `json:"games"`
	AvgPPR float32 `json:"avg_ppr"`
}

var recentStatKeys = []string{"rec_tgt", "rec", "rec_yd", "rec_td", "rush_att", "rush_yd", "rush_td",
	"pass_att", "pass_cmp", "pass_yd", "pass_td", "pass_int", "fum_lost", "pts_ppr"}

const (
	maxPredictPlayers = 20
	maxNewsBytes      = 1000
)

const predictDoc = "Predict this week's PPR range for up to 20 players: a decision model reads each player's last 3 games, " +
	"situation, opponent and your `news` notes (cite sources), and code scores its stat ranges with the league's settings. " +
	"Returns low/median/high (10th/50th/90th percentile; a Questionable player's sit-out chance included) and per-stat " +
	"[low,median,high]; `compare` pairs get p_a_outscores_b. A `note` instead of numbers means no prediction: estimate it yourself."

func (e *extension) predictTool() tool.Tool {
	run := func(ctx adkagent.Context, a predictArgs) (predictResult, error) { return e.predict(ctx, a) }
	t, _ := functiontool.New[predictArgs, predictResult](functiontool.Config{Name: "sleeper_predict", Description: predictDoc}, run)
	return t
}

// predictInputs is one call's shared reads, fetched once for every player.
type predictInputs struct {
	keys      decisionKeys
	scoring   map[string]float32
	dump      map[string]sleepergen.Player
	depth     teamDepth
	slate     slate
	recent    map[int]map[string]sleepergen.StatMap
	games     []sleepergen.Game
	byTeamPos map[string][]string
}

func (e *extension) predict(ctx context.Context, a predictArgs) (predictResult, error) {
	if len(a.Players) == 0 || len(a.Players) > maxPredictPlayers {
		return predictResult{}, fmt.Errorf("sleeper_predict: pass 1-%d players, got %d", maxPredictPlayers, len(a.Players))
	}
	in, cc, err := e.predictInputs(ctx, a.LeagueID)
	if err != nil {
		return predictResult{}, err
	}
	res := predictResult{callContext: cc}
	byID := map[string]*prediction{}
	for _, t := range a.Players {
		res.Players = append(res.Players, e.predictOne(ctx, in, t))
	}
	for i := range res.Players {
		byID[res.Players[i].PlayerID] = &res.Players[i]
	}
	for _, p := range a.Compare {
		pa, pb := byID[p.A], byID[p.B]
		if pa != nil && pb != nil && pa.grid != nil && pb.grid != nil {
			res.Compare = append(res.Compare, comparison{A: p.A, B: p.B, PAOutscores: pOutscores(pa.grid, pb.grid)})
		}
	}
	return res, nil
}

func (e *extension) predictInputs(ctx context.Context, leagueArg string) (predictInputs, callContext, error) {
	leagueID, err := e.resolveLeagueID(leagueArg)
	if err != nil {
		return predictInputs{}, callContext{}, err
	}
	league, err := e.client.League(ctx, leagueID)
	if err != nil {
		return predictInputs{}, callContext{}, fmt.Errorf("sleeper_predict: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return predictInputs{}, callContext{}, err
	}
	if state.SeasonType != "regular" {
		return predictInputs{}, callContext{}, fmt.Errorf("sleeper_predict: predicts regular-season weeks only (NFL %s season)", state.SeasonType)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return predictInputs{}, callContext{}, fmt.Errorf("sleeper_predict: %w", err)
	}
	c := e.newClock("")
	cc := newCallContext(c, state.Week, season, state)
	in := predictInputs{scoring: league.ScoringSettings, dump: dump, depth: newTeamDepth(dump),
		slate: e.playerSlate(ctx, &cc, season, state, c, true), recent: map[int]map[string]sleepergen.StatMap{},
		keys: decisionKeys{LeagueID: leagueID, Season: season, Week: state.Week}, byTeamPos: map[string][]string{}}
	if info, ok := sdk.CallInfoFrom(ctx); ok {
		in.keys.ChatID = info.ChatID
	}
	for w := state.Week - 1; w >= 1 && w >= state.Week-recentWeeks; w-- {
		if stats, err := e.client.WeekStats(ctx, season, w); err == nil {
			in.recent[w] = stats
		}
	}
	in.games, _ = e.client.Schedule(ctx, season)
	for id, p := range dump {
		if p.Team != nil && p.Position != nil {
			in.byTeamPos[*p.Team+"/"+*p.Position] = append(in.byTeamPos[*p.Team+"/"+*p.Position], id)
		}
	}
	return in, cc, nil
}

func (e *extension) predictOne(ctx context.Context, in predictInputs, t predictTarget) prediction {
	p, ok := in.dump[t.PlayerID]
	pos := strVal(p.Position)
	pred := prediction{PlayerID: t.PlayerID, Name: playerNameOf(p), Pos: pos}
	g := in.slate.gameFor(in.dump, t.PlayerID)
	play := playRate(strVal(p.InjuryStatus))
	line, modeled := lineFor[pos]
	switch {
	case !ok:
		pred.Note = "unknown player_id"
		return pred
	case g.Bye || play == 0:
		zero := []float64{0}
		pred.grid, pred.Low, pred.Median, pred.High = zero, new(float32), new(float32), new(float32)
		pred.Note = "on bye or ruled out: 0"
		return pred
	case !modeled:
		pred.Note = "no stat model for " + pos
		return pred
	case g.Game == nil:
		pred.Note = "no game joined: " + g.NoGameReason
		return pred
	}
	d, err := e.askLine(ctx, line, in.keys, e.predictState(in, t, p, g))
	if err != nil {
		pred.Note = err.Error()
		return pred
	}
	pred.fill(d, in.scoring, play)
	return pred
}

// askLine returns the stat distributions; only decide mode hands them to the agent, observe only records them.
func (e *extension) askLine(ctx context.Context, line string, keys decisionKeys, state predictState) ([]statDist, error) {
	if e.host.Decide == nil {
		return nil, fmt.Errorf("no decision model configured")
	}
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	d, err := e.host.Decide(ctx, sdk.DecideRequest{Point: line, State: state, ChatID: keys.ChatID})
	switch {
	case err != nil:
		return nil, fmt.Errorf("decision model unavailable: %w", err)
	case d.Outcome == "observe":
		return nil, fmt.Errorf("decision model is in observe mode")
	}
	var dists []statDist
	for _, q := range statLines[line] {
		if d.Probabilities[q.stat] == nil {
			return nil, fmt.Errorf("decision model gave no %s answer", q.stat)
		}
		dists = append(dists, statDist{q, d.Probabilities[q.stat]})
	}
	return dists, nil
}

func (pred *prediction) fill(dists []statDist, scoring map[string]float32, play float64) {
	pred.grid = pointsGrid(dists, scoring, pred.Pos, play)
	lo, med, hi := gridQuantile(pred.grid, 0.1), gridQuantile(pred.grid, 0.5), gridQuantile(pred.grid, 0.9)
	pred.Low, pred.Median, pred.High = &lo, &med, &hi
	pred.Stats = map[string][3]float32{}
	for _, d := range dists {
		pred.Stats[d.q.stat] = [3]float32{round1(quantile(d.probs, d.q.bands, 0.1)),
			round1(quantile(d.probs, d.q.bands, 0.5)), round1(quantile(d.probs, d.q.bands, 0.9))}
	}
}

func (e *extension) predictState(in predictInputs, t predictTarget, p sleepergen.Player, g gameInfo) predictState {
	env := in.depth.envOf(p)
	s := predictState{decisionKeys: in.keys, News: truncateBytes(t.News, maxNewsBytes),
		Player:       predictRow{ID: t.PlayerID, Name: playerNameOf(p), Pos: strVal(p.Position), Opp: g.Game.NFLOpponent, Home: g.Game.IsHome, playerEnv: env},
		TeamInjuries: in.depth.injuries([]string{env.NFLTeam})[env.NFLTeam],
		OppAllowed:   oppAllowed(g.Game.NFLOpponent, strVal(p.Position), in.games, in.recent, in.byTeamPos)}
	weeks := make([]int, 0, len(in.recent))
	for w := range in.recent {
		weeks = append(weeks, w)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(weeks)))
	for _, w := range weeks {
		if st, ok := in.recent[w][t.PlayerID]; ok {
			s.Recent = append(s.Recent, recentLine(w, st))
		}
	}
	return s
}

func recentLine(w int, st sleepergen.StatMap) statWeek {
	sw := statWeek{Week: w, Stats: map[string]float32{}}
	for _, k := range recentStatKeys {
		if v := st[k]; v != 0 {
			sw.Stats[k] = v
		}
	}
	if st["tm_off_snp"] > 0 {
		pct := float32(math.Round(float64(st["off_snp"] / st["tm_off_snp"] * 100)))
		sw.SnapPct = &pct
	}
	return sw
}

// oppAllowed averages what opp gave up to pos over the recent weeks; byTeamPos holds today's teams, so a traded player
// counts for his new team.
func oppAllowed(opp, pos string, games []sleepergen.Game, recent map[int]map[string]sleepergen.StatMap, byTeamPos map[string][]string) *allowed {
	a := allowed{Pos: pos}
	total := float32(0)
	for _, g := range games {
		stats, ok := recent[g.Week]
		if !ok || (g.Home != opp && g.Away != opp) {
			continue
		}
		faced := g.Home
		if faced == opp {
			faced = g.Away
		}
		for _, id := range byTeamPos[faced+"/"+pos] {
			total += stats[id]["pts_ppr"]
		}
		a.Games++
	}
	if a.Games == 0 {
		return nil
	}
	a.AvgPPR = round1(float64(total) / float64(a.Games))
	return &a
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
