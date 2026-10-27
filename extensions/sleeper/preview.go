package sleeper

import (
	"context"
	"strings"

	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/extensions/sleeper/sleepergen"
)

// projectionsHidden: the digest previews from news and recent games, so its calls never see Sleeper's projections.
func projectionsHidden(ctx context.Context) bool {
	ci, ok := sdk.CallInfoFrom(ctx)
	return ok && strings.HasPrefix(ci.ChatID, "ext:sleeper:") && strings.HasSuffix(ci.ChatID, ":digest")
}

// weekProjections is nil, without a fetch, when this call's projections are hidden.
func (e *extension) weekProjections(ctx context.Context, season string, week int) (map[string]sleepergen.StatMap, error) {
	if projectionsHidden(ctx) {
		return nil, nil
	}
	return e.client.WeekProjections(ctx, season, week)
}

// projectionOf is nil for hidden projections, so the field drops out rather than reading as 0.
func projectionOf(proj map[string]sleepergen.StatMap, pid string) *float32 {
	if proj == nil {
		return nil
	}
	v := proj[pid]["pts_ppr"]
	return &v
}

// teamWeek is one of a fantasy team's completed weeks.
type teamWeek struct {
	Week      int     `json:"week"`
	Points    float32 `json:"points"`
	OppPoints float32 `json:"opp_points"`
	Result    string  `json:"result"`
}

// addRecentForm gives a preview each side's last recentWeeks results and each starter's recent points,
// averaged in code so the digest copies numbers instead of summing them.
func (e *extension) addRecentForm(ctx context.Context, leagueID, season string, week int, sides ...*side) {
	for w := week - 1; w >= 1 && w >= week-recentWeeks; w-- {
		matchups, err := e.client.Matchups(ctx, leagueID, w)
		if err != nil {
			e.logWarn("sleeper: recent matchups fetch failed", "league_id", leagueID, "week", w, "error", err)
			continue
		}
		stats, err := e.client.WeekStats(ctx, season, w)
		if err != nil {
			e.logWarn("sleeper: recent stats fetch failed", "season", season, "week", w, "error", err)
		}
		for _, s := range sides {
			addWeekForm(s, w, matchups, stats)
		}
	}
	for _, s := range sides {
		s.RecentAvg = avgPoints(s.RecentScores, func(t teamWeek) float32 { return t.Points })
		for i := range s.Starters {
			s.Starters[i].RecentAvg = avgPoints(s.Starters[i].Recent, func(p weekPts) float32 { return p.PtsPPR })
		}
	}
}

func addWeekForm(s *side, w int, matchups []sleepergen.Matchup, stats map[string]sleepergen.StatMap) {
	if m, ok := findMatchup(matchups, s.RosterID); ok {
		tw := teamWeek{Week: w, Points: m.Points, Result: "-"}
		if opp, ok := opponent(matchups, m); ok {
			tw.OppPoints = opp.Points
			tw.Result, _ = weekResult(m.Points, opp.Points)
		}
		s.RecentScores = append(s.RecentScores, tw)
	}
	for i, st := range s.Starters {
		if row, ok := stats[st.PlayerID]; ok {
			s.Starters[i].Recent = append(s.Starters[i].Recent, weekPts{Week: w, PtsPPR: row["pts_ppr"]})
		}
	}
}

func avgPoints[T any](rows []T, pts func(T) float32) float32 {
	if len(rows) == 0 {
		return 0
	}
	var sum float32
	for _, r := range rows {
		sum += pts(r)
	}
	return round2(sum / float32(len(rows)))
}
