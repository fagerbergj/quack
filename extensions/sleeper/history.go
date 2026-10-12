// sleeper_history: per season, a draft report card, lineup efficiency, weekly hindsight, and the champion.
package sleeper

import (
	"context"
	"fmt"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// closeLossMargin: a loss decided by fewer than this many points is "close".
const closeLossMargin = 10.0

// reachStealThreshold: a positional finish this far off the drafted-as rank is a reach or steal, not variance.
const reachStealThreshold = 5

// maxHistoryWeeks bounds the hindsight loop; no season runs past week 18.
const maxHistoryWeeks = 18

type historyArgs struct {
	LeagueID    string `json:"league_id,omitempty"`
	User        string `json:"user,omitempty"`
	SeasonsBack int    `json:"seasons_back,omitempty"`
}

type draftReportCardEntry struct {
	PlayerID      string `json:"player_id"`
	Name          string `json:"name"`
	Position      string `json:"position"`
	Round         int    `json:"round"`
	PickNo        int    `json:"pick_no"`
	DraftedAsRank int    `json:"drafted_as_rank"`
	FinishRank    int    `json:"finish_rank"`
	Verdict       string `json:"verdict"` // "reach" | "steal" | "as_expected"
}

type weekHindsight struct {
	Week         int     `json:"week"`
	Result       string  `json:"result"` // "W" | "L" | "T"
	Started      float32 `json:"started"`
	BestPossible float32 `json:"best_possible"`
	PointsLeft   float32 `json:"points_left"`
	CloseLoss    bool    `json:"close_loss"`
}

type seasonHistory struct {
	Season              string                 `json:"season"`
	LeagueID            string                 `json:"league_id"`
	Wins                int                    `json:"wins"`
	Losses              int                    `json:"losses"`
	Ties                int                    `json:"ties"`
	LineupEfficiencyPct float64                `json:"lineup_efficiency_pct"`
	CloseLosses         int                    `json:"close_losses"`
	Champion            string                 `json:"champion,omitempty"`
	Draft               []draftReportCardEntry `json:"draft,omitempty"`
	Weeks               []weekHindsight        `json:"weeks,omitempty"`
}

type historyResult struct {
	Seasons          []seasonHistory `json:"seasons"`
	TotalCloseLosses int             `json:"total_close_losses"`
	FetchedAt        string          `json:"fetched_at"`
}

func (e *extension) historyTool() tool.Tool {
	t, _ := functiontool.New[historyArgs, historyResult](
		functiontool.Config{
			Name: "sleeper_history",
			Description: "Walk a league's chain (previous_league_id) and report, per season: a draft " +
				"report card (drafted-as rank vs positional finish), lineup efficiency (started vs best " +
				"possible), close losses (decided by fewer than 10 points), and the champion. " +
				"`seasons_back` caps how many seasons back from league_id to include (default all available).",
		},
		func(ctx adkagent.Context, a historyArgs) (historyResult, error) { return e.getHistory(ctx, a) },
	)
	return t
}

func (e *extension) getHistory(ctx context.Context, a historyArgs) (historyResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return historyResult{}, err
	}
	uid, err := e.resolveUserIdentifier(a.User)
	if err != nil {
		return historyResult{}, err
	}
	chain := e.client.Chain(ctx, leagueID, a.SeasonsBack)
	if len(chain) == 0 {
		return historyResult{}, fmt.Errorf("sleeper_history: no league found for %s", leagueID)
	}
	out := historyResult{FetchedAt: nowRFC3339()}
	for _, league := range chain {
		sh, err := e.seasonHistoryFor(ctx, league, uid)
		if err != nil {
			return historyResult{}, fmt.Errorf("sleeper_history: season %s: %w", league.Season, err)
		}
		out.Seasons = append(out.Seasons, sh)
		out.TotalCloseLosses += sh.CloseLosses
	}
	return out, nil
}

func (e *extension) seasonHistoryFor(ctx context.Context, league sleepergen.League, uid string) (seasonHistory, error) {
	rosters, err := e.client.Rosters(ctx, league.LeagueId)
	if err != nil {
		return seasonHistory{}, fmt.Errorf("rosters: %w", err)
	}
	// rosterIDForUser can match owner_id without users, so a failure here only degrades names to "roster N".
	users, _ := e.client.LeagueUsers(ctx, league.LeagueId)
	myRosterID, ok := rosterIDForUser(rosters, users, uid)
	if !ok {
		return seasonHistory{}, fmt.Errorf("no roster found for %q", uid)
	}
	myRoster, _ := rosterFor(rosters, myRosterID)
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return seasonHistory{}, err
	}
	weeks, closeLosses := e.weeklyHindsight(ctx, league, myRosterID, dump)
	sh := seasonHistory{
		Season: league.Season, LeagueID: league.LeagueId,
		Wins: myRoster.Settings["wins"], Losses: myRoster.Settings["losses"], Ties: myRoster.Settings["ties"],
		LineupEfficiencyPct: lineupEfficiency(myRoster.Settings),
		CloseLosses:         closeLosses,
		Weeks:               weeks,
	}
	sh.Champion, err = e.champion(ctx, league.LeagueId, rosters, users)
	if err != nil {
		return seasonHistory{}, err
	}
	sh.Draft, err = e.draftReportCard(ctx, league, myRosterID)
	if err != nil {
		return seasonHistory{}, err
	}
	return sh, nil
}

func lineupEfficiency(settings map[string]int) float64 {
	ppts := pointsField(settings, "ppts")
	if ppts == 0 {
		return 0
	}
	fpts := pointsField(settings, "fpts")
	return roundTo1(fpts / ppts * 100)
}

func roundTo1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func (e *extension) champion(ctx context.Context, leagueID string, rosters []sleepergen.Roster, users []sleepergen.LeagueUser) (string, error) {
	bracket, err := e.client.WinnersBracket(ctx, leagueID)
	if err != nil {
		return "", fmt.Errorf("winners bracket: %w", err)
	}
	names := teamNames(rosters, users)
	for _, m := range bracket {
		if m.P != nil && *m.P == 1 && m.W != nil {
			return names[*m.W], nil
		}
	}
	return "", nil
}

// weeklyHindsight walks every week with matchup data; a missing week is skipped, not an error.
func (e *extension) weeklyHindsight(ctx context.Context, league sleepergen.League, myRosterID int, dump map[string]sleepergen.Player) ([]weekHindsight, int) {
	lastWeek := maxHistoryWeeks
	// An in-progress season's current week is still accruing points, so stop before it.
	if league.Status != "complete" {
		if state, err := e.client.State(ctx); err == nil && state.Week-1 < lastWeek {
			lastWeek = state.Week - 1
		}
	}
	var weeks []weekHindsight
	closeLosses := 0
	for week := 1; week <= lastWeek; week++ {
		matchups, err := e.client.Matchups(ctx, league.LeagueId, week)
		if err != nil || len(matchups) == 0 {
			continue
		}
		mine, ok := findMatchup(matchups, myRosterID)
		if !ok {
			continue
		}
		opp, hasOpp := opponent(matchups, mine)
		w := hindsightFor(week, league.RosterPositions, mine, dump)
		if hasOpp {
			w.Result, w.CloseLoss = weekResult(mine.Points, opp.Points)
			if w.CloseLoss {
				closeLosses++
			}
		}
		weeks = append(weeks, w)
	}
	return weeks, closeLosses
}

func weekResult(mine, opp float32) (result string, close bool) {
	switch {
	case mine > opp:
		result = "W"
	case mine < opp:
		result = "L"
	default:
		result = "T"
	}
	return result, mine < opp && opp-mine < closeLossMargin
}

func hindsightFor(week int, rosterPositions []string, m sleepergen.Matchup, dump map[string]sleepergen.Player) weekHindsight {
	best := bestLineup(rosterPositions, m.Players, m.PlayersPoints, dump).total
	return weekHindsight{
		Week: week, Started: m.Points, BestPossible: best, PointsLeft: best - m.Points,
	}
}

// draftReportCard grades the owner's picks: drafted-as rank vs finish rank, both within position.
func (e *extension) draftReportCard(ctx context.Context, league sleepergen.League, myRosterID int) ([]draftReportCardEntry, error) {
	if league.DraftId == nil {
		return nil, nil
	}
	picks, err := e.client.DraftPicks(ctx, *league.DraftId)
	if err != nil {
		return nil, fmt.Errorf("draft picks: %w", err)
	}
	// An in-progress season has no season-total stats, so finish rank isn't knowable.
	if league.Status != "complete" {
		return nil, nil
	}
	stats, err := e.client.SeasonStats(ctx, league.Season)
	if err != nil {
		return nil, fmt.Errorf("season stats: %w", err)
	}
	draftedRank := positionalDraftRank(picks)
	finishRank := positionalFinishRank(picks, stats)
	var out []draftReportCardEntry
	for _, p := range picks {
		if p.RosterId != myRosterID {
			continue
		}
		dr, fr := draftedRank[p.PlayerId], finishRank[p.PlayerId]
		out = append(out, draftReportCardEntry{
			PlayerID: p.PlayerId, Name: pickPlayerName(p), Position: pickPosition(p),
			Round: p.Round, PickNo: p.PickNo, DraftedAsRank: dr, FinishRank: fr,
			Verdict: reachOrSteal(dr, fr),
		})
	}
	return out, nil
}

func reachOrSteal(draftedAsRank, finishRank int) string {
	if draftedAsRank == 0 || finishRank == 0 {
		return "unknown"
	}
	diff := finishRank - draftedAsRank
	switch {
	case diff >= reachStealThreshold:
		return "reach"
	case diff <= -reachStealThreshold:
		return "steal"
	default:
		return "as_expected"
	}
}

func picksByPosition(picks []sleepergen.DraftPick) map[string][]sleepergen.DraftPick {
	byPos := map[string][]sleepergen.DraftPick{}
	for _, p := range picks {
		pos := pickPosition(p)
		byPos[pos] = append(byPos[pos], p)
	}
	return byPos
}

// positionalDraftRank ranks every pick within its position by pick_no.
func positionalDraftRank(picks []sleepergen.DraftPick) map[string]int {
	out := map[string]int{}
	for _, group := range picksByPosition(picks) {
		sort.Slice(group, func(i, j int) bool { return group[i].PickNo < group[j].PickNo })
		for i, p := range group {
			out[p.PlayerId] = i + 1
		}
	}
	return out
}

// positionalFinishRank ranks every drafted player within its position by season pts_ppr, highest first.
func positionalFinishRank(picks []sleepergen.DraftPick, stats map[string]sleepergen.StatMap) map[string]int {
	out := map[string]int{}
	for _, group := range picksByPosition(picks) {
		sort.SliceStable(group, func(i, j int) bool {
			return stats[group[i].PlayerId]["pts_ppr"] > stats[group[j].PlayerId]["pts_ppr"]
		})
		for i, p := range group {
			out[p.PlayerId] = i + 1
		}
	}
	return out
}
