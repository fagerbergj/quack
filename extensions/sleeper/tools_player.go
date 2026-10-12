package sleeper

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// maxGameLogWeeks caps game_log: a floor/ceiling read needs recent usage, not the whole season.
const maxGameLogWeeks = 5

type playerArgs struct {
	PlayerID string `json:"player_id,omitempty"`
	Name     string `json:"name,omitempty"`
	TZ       string `json:"tz,omitempty"`
}

type playerResult struct {
	callContext
	PlayerID            string             `json:"player_id"`
	Name                string             `json:"name"`
	Position            string             `json:"position,omitempty"`
	Team                string             `json:"team,omitempty"`
	DepthChartOrder     int                `json:"depth_chart_order,omitempty"`
	InjuryStatus        string             `json:"injury_status,omitempty"`
	PracticeDescription string             `json:"practice_description,omitempty"`
	NewsUpdated         int                `json:"news_updated,omitempty"`
	SeasonStats         map[string]float32 `json:"season_stats,omitempty"`
	WeekProjection      float32            `json:"week_projection"`
	GameLog             []gameLogWeek      `json:"game_log,omitempty"`
	gameInfo
}

// gameLogWeek is one completed week; floor/ceiling reads snap share and volume trend from it.
type gameLogWeek struct {
	Week    int     `json:"week"`
	Opp     string  `json:"opp,omitempty"`
	SnapPct *int    `json:"snap_pct,omitempty"`
	Targets float32 `json:"targets"`
	Carries float32 `json:"carries"`
	PtsPPR  float32 `json:"pts_ppr"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) playerTool() tool.Tool {
	t, _ := functiontool.New[playerArgs, playerResult](
		functiontool.Config{
			Name: "sleeper_player",
			Description: "Get one player's bio, team, depth chart order, injury/practice status, news " +
				"timestamp, season stats, and the current NFL week's projection. Look up by `player_id` or " +
				"`name`. " + playerGameDoc + " " + timeDoc,
		},
		func(ctx adkagent.Context, a playerArgs) (playerResult, error) { return e.getPlayer(ctx, a) },
	)
	return t
}

func (e *extension) getPlayer(ctx context.Context, a playerArgs) (playerResult, error) {
	c := e.newClock(a.TZ)
	p, err := e.resolvePlayer(ctx, a.PlayerID, a.Name)
	if err != nil {
		return playerResult{}, fmt.Errorf("sleeper_player: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return playerResult{}, err
	}
	week := state.Week
	proj, err := e.client.WeekProjections(ctx, season, week)
	if err != nil {
		return playerResult{}, fmt.Errorf("sleeper_player: projections: %w", err)
	}
	cc := newCallContext(c, week, season, state)
	sl := e.playerSlate(ctx, &cc, season, state, c, true)
	var stats map[string]float32
	if entry, err := e.client.PlayerSeasonStats(ctx, p.PlayerId, season); err == nil && entry.Stats != nil {
		stats = *entry.Stats
	}
	var gameLog []gameLogWeek
	if entries, err := e.client.PlayerGameLog(ctx, p.PlayerId, season); err == nil {
		gameLog = buildGameLog(entries, week)
	}
	res := playerResult{
		PlayerID: p.PlayerId, Name: playerNameOf(p), Position: strVal(p.Position), Team: strVal(p.Team),
		DepthChartOrder: intVal(p.DepthChartOrder), InjuryStatus: strVal(p.InjuryStatus),
		PracticeDescription: strVal(p.PracticeDescription), NewsUpdated: intVal(p.NewsUpdated),
		SeasonStats: stats, WeekProjection: proj[p.PlayerId]["pts_ppr"], GameLog: gameLog,
		gameInfo: sl.gameFor(map[string]sleepergen.Player{p.PlayerId: p}, p.PlayerId),
	}
	res.callContext = cc
	return res, nil
}

// buildGameLog keeps weeks before currentWeek (whose stats aren't final), newest first, capped.
func buildGameLog(entries []sleepergen.PlayerStatEntry, currentWeek int) []gameLogWeek {
	var out []gameLogWeek
	for _, e := range entries {
		if e.Week == nil || *e.Week >= currentWeek {
			continue
		}
		out = append(out, gameLogWeekFrom(*e.Week, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Week > out[j].Week })
	if len(out) > maxGameLogWeeks {
		out = out[:maxGameLogWeeks]
	}
	return out
}

func gameLogWeekFrom(week int, e sleepergen.PlayerStatEntry) gameLogWeek {
	var stats map[string]float32
	if e.Stats != nil {
		stats = *e.Stats
	}
	return gameLogWeek{
		Week: week, Opp: strVal(e.Opponent), SnapPct: snapPct(stats),
		Targets: stats["rec_tgt"], Carries: stats["rush_att"], PtsPPR: stats["pts_ppr"],
	}
}

// snapPct is off_snp/tm_off_snp as a whole percent, nil when tm_off_snp is missing or 0.
func snapPct(stats map[string]float32) *int {
	tm := stats["tm_off_snp"]
	if tm <= 0 {
		return nil
	}
	pct := int(math.Round(float64(stats["off_snp"]/tm) * 100))
	return &pct
}

// resolvePlayer looks a player up by id (cheap single lookup) or by name (the dump's index).
func (e *extension) resolvePlayer(ctx context.Context, playerID, name string) (sleepergen.Player, error) {
	if playerID != "" {
		p, err := e.client.Player(ctx, playerID)
		if err != nil {
			return sleepergen.Player{}, err
		}
		return *p, nil
	}
	if name == "" {
		return sleepergen.Player{}, fmt.Errorf("player_id or name is required")
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return sleepergen.Player{}, err
	}
	ids := e.client.ResolvePlayer(name)
	if len(ids) == 0 {
		return sleepergen.Player{}, fmt.Errorf("no player found matching %q", name)
	}
	return dump[ids[0]], nil
}

func playerNameOf(p sleepergen.Player) string {
	if p.FullName != nil && *p.FullName != "" {
		return *p.FullName
	}
	if full := strings.TrimSpace(strVal(p.FirstName) + " " + strVal(p.LastName)); full != "" {
		return full
	}
	if p.Position != nil && *p.Position == "DEF" {
		return strVal(p.Team)
	}
	return p.PlayerId
}

func intVal(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
