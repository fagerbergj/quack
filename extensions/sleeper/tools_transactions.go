package sleeper

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

type transactionsArgs struct {
	LeagueID  string `json:"league_id,omitempty"`
	WeeksBack int    `json:"weeks_back,omitempty"`
}

// moveEntry is one player added or dropped, with its roster so a multi-team trade stays attributable.
type moveEntry struct {
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
	Position string `json:"position"`
	Team     string `json:"team"`
}

type transactionEntry struct {
	Week    int         `json:"week"`
	Type    string      `json:"type"`
	Status  string      `json:"status"`
	Teams   []string    `json:"teams"`
	Adds    []moveEntry `json:"adds,omitempty"`
	Drops   []moveEntry `json:"drops,omitempty"`
	FaabBid int         `json:"faab_bid,omitempty"`
}

type transactionsResult struct {
	LeagueID     string             `json:"league_id"`
	Transactions []transactionEntry `json:"transactions"`
	FetchedAt    string             `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) transactionsTool() tool.Tool {
	t, _ := functiontool.New[transactionsArgs, transactionsResult](
		functiontool.Config{
			Name: "sleeper_transactions",
			Description: "Get recent trades, waivers, and free-agent adds/drops for a league, with FAAB " +
				"bids and the team name per roster. `weeks_back` (default 1) sets how many weeks back from " +
				"the current week to look.",
		},
		func(ctx adkagent.Context, a transactionsArgs) (transactionsResult, error) {
			return e.getTransactions(ctx, a)
		},
	)
	return t
}

func (e *extension) getTransactions(ctx context.Context, a transactionsArgs) (transactionsResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return transactionsResult{}, err
	}
	weeksBack := a.WeeksBack
	if weeksBack <= 0 {
		weeksBack = 1
	}
	rosters, err := e.client.Rosters(ctx, leagueID)
	if err != nil {
		return transactionsResult{}, fmt.Errorf("sleeper_transactions: rosters: %w", err)
	}
	users, err := e.client.LeagueUsers(ctx, leagueID)
	if err != nil {
		return transactionsResult{}, fmt.Errorf("sleeper_transactions: league users: %w", err)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return transactionsResult{}, fmt.Errorf("sleeper_transactions: %w", err)
	}
	_, state, err := e.season(ctx)
	if err != nil {
		return transactionsResult{}, err
	}
	currentWeek := state.Week
	names := teamNames(rosters, users)
	var out []transactionEntry
	for week := currentWeek - weeksBack + 1; week <= currentWeek; week++ {
		if week < 1 {
			continue
		}
		txs, err := e.client.Transactions(ctx, leagueID, week)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // no round recorded for this week yet
			}
			return transactionsResult{}, fmt.Errorf("sleeper_transactions: week %d: %w", week, err)
		}
		for _, tx := range txs {
			out = append(out, buildTransactionEntry(week, tx, names, dump))
		}
	}
	return transactionsResult{LeagueID: leagueID, Transactions: out, FetchedAt: nowRFC3339()}, nil
}

func buildTransactionEntry(week int, tx sleepergen.Transaction, names map[int]string, dump map[string]sleepergen.Player) transactionEntry {
	teams := make([]string, 0, len(tx.RosterIds))
	for _, rid := range tx.RosterIds {
		teams = append(teams, names[rid])
	}
	entry := transactionEntry{
		Week: week, Type: tx.Type, Status: tx.Status, Teams: teams,
		Adds: playerMoves(tx.Adds, names, dump), Drops: playerMoves(tx.Drops, names, dump),
	}
	if tx.Settings != nil {
		entry.FaabBid = (*tx.Settings)["waiver_bid"]
	}
	return entry
}

// playerMoves turns a player_id -> roster_id map into named, team-attributed moves.
func playerMoves(m *map[string]int, names map[int]string, dump map[string]sleepergen.Player) []moveEntry {
	if m == nil {
		return nil
	}
	out := make([]moveEntry, 0, len(*m))
	for pid, rid := range *m {
		out = append(out, moveEntry{
			PlayerID: pid, Name: playerName(dump, pid), Position: playerPosition(dump, pid), Team: names[rid],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlayerID < out[j].PlayerID })
	return out
}

type freeAgentsArgs struct {
	LeagueID string `json:"league_id,omitempty"`
	Position string `json:"position,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type freeAgent struct {
	PlayerID     string  `json:"player_id"`
	Name         string  `json:"name"`
	Position     string  `json:"position"`
	Team         string  `json:"team"`
	Projection   float32 `json:"projection"`
	TrendingAdds int     `json:"trending_adds"`
	OwnedPct     float32 `json:"owned_pct"`
}

type freeAgentsResult struct {
	LeagueID   string      `json:"league_id"`
	FreeAgents []freeAgent `json:"free_agents"`
	FetchedAt  string      `json:"fetched_at"`
}

const defaultFreeAgentLimit = 20

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) freeAgentsTool() tool.Tool {
	t, _ := functiontool.New[freeAgentsArgs, freeAgentsResult](
		functiontool.Config{
			Name: "sleeper_free_agents",
			Description: "Rank unrostered players by this week's projection, with trending-add counts and " +
				"research ownership percentage. `position` filters (e.g. \"RB\"); `limit` caps the list (default 20).",
		},
		func(ctx adkagent.Context, a freeAgentsArgs) (freeAgentsResult, error) { return e.getFreeAgents(ctx, a) },
	)
	return t
}

func (e *extension) getFreeAgents(ctx context.Context, a freeAgentsArgs) (freeAgentsResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return freeAgentsResult{}, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultFreeAgentLimit
	}
	rosters, err := e.client.Rosters(ctx, leagueID)
	if err != nil {
		return freeAgentsResult{}, fmt.Errorf("sleeper_free_agents: rosters: %w", err)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return freeAgentsResult{}, fmt.Errorf("sleeper_free_agents: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return freeAgentsResult{}, err
	}
	week := state.Week
	proj, err := e.client.WeekProjections(ctx, season, week)
	if err != nil {
		return freeAgentsResult{}, fmt.Errorf("sleeper_free_agents: projections: %w", err)
	}
	trending, err := e.client.TrendingPlayers(ctx, sleepergen.Add, 24, 25)
	if err != nil {
		return freeAgentsResult{}, fmt.Errorf("sleeper_free_agents: trending: %w", err)
	}
	research, err := e.client.Research(ctx, season, week)
	if err != nil {
		return freeAgentsResult{}, fmt.Errorf("sleeper_free_agents: research: %w", err)
	}
	agents := rankFreeAgents(rostered(rosters), dump, proj, trendingCounts(trending), research, a.Position)
	if len(agents) > limit {
		agents = agents[:limit]
	}
	return freeAgentsResult{LeagueID: leagueID, FreeAgents: agents, FetchedAt: nowRFC3339()}, nil
}

func rostered(rosters []sleepergen.Roster) map[string]bool {
	out := map[string]bool{}
	for _, r := range rosters {
		for _, pid := range r.Players {
			out[pid] = true
		}
	}
	return out
}

func trendingCounts(trending []sleepergen.TrendingPlayer) map[string]int {
	out := make(map[string]int, len(trending))
	for _, t := range trending {
		out[t.PlayerId] = t.Count
	}
	return out
}

func rankFreeAgents(rostered map[string]bool, dump map[string]sleepergen.Player, proj map[string]sleepergen.StatMap, trending map[string]int, research map[string]sleepergen.ResearchEntry, position string) []freeAgent {
	ids := make([]string, 0, len(dump))
	for pid, p := range dump {
		if rostered[pid] || !matchesPosition(p, position) {
			continue
		}
		ids = append(ids, pid)
	}
	ids = sortedByProjection(ids, proj)
	out := make([]freeAgent, len(ids))
	for i, pid := range ids {
		var owned float32
		if r, ok := research[pid]; ok && r.Owned != nil {
			owned = *r.Owned
		}
		p := dump[pid]
		out[i] = freeAgent{
			PlayerID: pid, Name: playerName(dump, pid), Position: strVal(p.Position), Team: strVal(p.Team),
			Projection: proj[pid]["pts_ppr"], TrendingAdds: trending[pid], OwnedPct: owned,
		}
	}
	return out
}

func matchesPosition(p sleepergen.Player, position string) bool {
	if position == "" {
		return true
	}
	if p.Position != nil && *p.Position == position {
		return true
	}
	return p.FantasyPositions != nil && slices.Contains(*p.FantasyPositions, position)
}
