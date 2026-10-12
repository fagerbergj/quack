package sleeper

import (
	"context"
	"fmt"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type standingsArgs struct {
	LeagueID string `json:"league_id,omitempty"`
}

type standingsRow struct {
	Rank      int     `json:"rank"`
	RosterID  int     `json:"roster_id"`
	Team      string  `json:"team"`
	Wins      int     `json:"wins"`
	Losses    int     `json:"losses"`
	Ties      int     `json:"ties"`
	Fpts      float64 `json:"fpts"`
	FptsAgnst float64 `json:"fpts_against"`
	InPlayoff bool    `json:"in_playoff_line"`
}

type standingsResult struct {
	LeagueID       string         `json:"league_id"`
	Standings      []standingsRow `json:"standings"`
	RemainingWeeks int            `json:"remaining_weeks"`
	FetchedAt      string         `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) standingsTool() tool.Tool {
	t, _ := functiontool.New[standingsArgs, standingsResult](
		functiontool.Config{
			Name: "sleeper_standings",
			Description: "Get league standings: records, points for/against, and the playoff line, " +
				"ranked by wins then points for. Falls back to default_league.",
		},
		func(ctx adkagent.Context, a standingsArgs) (standingsResult, error) { return e.getStandings(ctx, a) },
	)
	return t
}

func (e *extension) getStandings(ctx context.Context, a standingsArgs) (standingsResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return standingsResult{}, err
	}
	league, rosters, users, err := e.leagueRostersUsers(ctx, leagueID)
	if err != nil {
		return standingsResult{}, err
	}
	state, err := e.client.State(ctx)
	if err != nil {
		return standingsResult{}, fmt.Errorf("sleeper_standings: state: %w", err)
	}
	names := teamNames(rosters, users)
	playoffTeams := league.Settings["playoff_teams"]
	rows := make([]standingsRow, len(rosters))
	for i, r := range rosters {
		rows[i] = standingsRow{
			RosterID: r.RosterId, Team: names[r.RosterId],
			Wins: r.Settings["wins"], Losses: r.Settings["losses"], Ties: r.Settings["ties"],
			Fpts:      pointsField(r.Settings, "fpts"),
			FptsAgnst: pointsField(r.Settings, "fpts_against"),
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Wins != rows[j].Wins {
			return rows[i].Wins > rows[j].Wins
		}
		return rows[i].Fpts > rows[j].Fpts
	})
	for i := range rows {
		rows[i].Rank = i + 1
		rows[i].InPlayoff = rows[i].Rank <= playoffTeams
	}
	remaining := league.Settings["playoff_week_start"] - state.Week
	if remaining < 0 {
		remaining = 0
	}
	return standingsResult{LeagueID: leagueID, Standings: rows, RemainingWeeks: remaining, FetchedAt: nowRFC3339()}, nil
}
