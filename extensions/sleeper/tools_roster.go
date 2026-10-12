package sleeper

import (
	"context"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// pointsField joins Sleeper's split points pair, e.g. settings["fpts"] and settings["fpts_decimal"].
func pointsField(settings map[string]int, prefix string) float64 {
	return float64(settings[prefix]) + float64(settings[prefix+"_decimal"])/100
}

type playerRef struct {
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
}

type slotPlayer struct {
	Slot     string `json:"slot,omitempty"`
	PlayerID string `json:"player_id"`
	Name     string `json:"name"`
	gameInfo
}

type rosterArgs struct {
	LeagueID string `json:"league_id,omitempty"`
	User     string `json:"user,omitempty"`
	RosterID int    `json:"roster_id,omitempty"`
	TZ       string `json:"tz,omitempty"`
}

type rosterResult struct {
	callContext
	RosterID  int          `json:"roster_id"`
	Team      string       `json:"team"`
	Starters  []slotPlayer `json:"starters"`
	Bench     []slotPlayer `json:"bench"`
	Reserve   []playerRef  `json:"reserve,omitempty"`
	Taxi      []playerRef  `json:"taxi,omitempty"`
	Wins      int          `json:"wins"`
	Losses    int          `json:"losses"`
	Ties      int          `json:"ties"`
	Fpts      float64      `json:"fpts"`
	WaiverPos int          `json:"waiver_position"`
	FaabUsed  int          `json:"faab_used"`
	FaabLeft  int          `json:"faab_left"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) rosterTool() tool.Tool {
	t, _ := functiontool.New[rosterArgs, rosterResult](
		functiontool.Config{
			Name: "sleeper_roster",
			Description: "Get a roster for the current NFL week: starters by slot, bench, reserve/taxi, " +
				"record, fpts, waiver position, FAAB used/left. " + playerGameDoc + " `league_id` falls " +
				"back to default_league; `user` or `roster_id` picks the team, falling back to default_user. " +
				"Reserve (IR) and taxi players carry no game. " + timeDoc,
		},
		func(ctx adkagent.Context, a rosterArgs) (rosterResult, error) { return e.getRoster(ctx, a) },
	)
	return t
}

func (e *extension) getRoster(ctx context.Context, a rosterArgs) (rosterResult, error) {
	c := e.newClock(a.TZ)
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return rosterResult{}, err
	}
	league, rosters, users, err := e.leagueRostersUsers(ctx, leagueID)
	if err != nil {
		return rosterResult{}, err
	}
	rosterID, err := e.resolveRosterID(a.RosterID, a.User, rosters, users)
	if err != nil {
		return rosterResult{}, fmt.Errorf("sleeper_roster: %w", err)
	}
	r, ok := rosterFor(rosters, rosterID)
	if !ok {
		return rosterResult{}, fmt.Errorf("sleeper_roster: roster %d not found", rosterID)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return rosterResult{}, fmt.Errorf("sleeper_roster: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return rosterResult{}, err
	}
	week := state.Week
	cc := newCallContext(c, week, season, state)
	sl := e.playerSlate(ctx, &cc, season, state, c, true)
	res := buildRosterResult(league, r, dump, sl, teamNames(rosters, users))
	res.callContext = cc
	return res, nil
}

func (e *extension) leagueRostersUsers(ctx context.Context, leagueID string) (*sleepergen.League, []sleepergen.Roster, []sleepergen.LeagueUser, error) {
	league, err := e.client.League(ctx, leagueID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("league: %w", err)
	}
	rosters, err := e.client.Rosters(ctx, leagueID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rosters: %w", err)
	}
	users, err := e.client.LeagueUsers(ctx, leagueID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("league users: %w", err)
	}
	return league, rosters, users, nil
}

func buildRosterResult(league *sleepergen.League, r sleepergen.Roster, dump map[string]sleepergen.Player, sl slate, names map[int]string) rosterResult {
	starterSlots := nonBenchSlots(league.RosterPositions)
	starters := make([]slotPlayer, 0, len(r.Starters))
	onField := map[string]bool{}
	for i, pid := range r.Starters {
		slot := "?"
		if i < len(starterSlots) {
			slot = starterSlots[i]
		}
		onField[pid] = true
		starters = append(starters, slotPlayer{Slot: slot, PlayerID: pid, Name: playerName(dump, pid), gameInfo: sl.gameFor(dump, pid)})
	}
	reserveSet, taxiSet := toSet(r.Reserve), toSet(r.Taxi)
	var bench []slotPlayer
	var reserve, taxi []playerRef
	for _, pid := range r.Players {
		if onField[pid] {
			continue
		}
		ref := playerRef{PlayerID: pid, Name: playerName(dump, pid)}
		switch {
		case reserveSet[pid]:
			reserve = append(reserve, ref)
		case taxiSet[pid]:
			taxi = append(taxi, ref)
		default:
			bench = append(bench, slotPlayer{PlayerID: pid, Name: ref.Name, gameInfo: sl.gameFor(dump, pid)})
		}
	}
	return rosterResult{
		RosterID: r.RosterId, Team: names[r.RosterId],
		Starters: starters, Bench: bench, Reserve: reserve, Taxi: taxi,
		Wins: r.Settings["wins"], Losses: r.Settings["losses"], Ties: r.Settings["ties"],
		Fpts:      pointsField(r.Settings, "fpts"),
		WaiverPos: r.Settings["waiver_position"],
		FaabUsed:  r.Settings["waiver_budget_used"],
		FaabLeft:  league.Settings["waiver_budget"] - r.Settings["waiver_budget_used"],
	}
}

// nonBenchSlots strips BN/IR/TAXI, leaving starter slots in the order Roster.Starters is indexed by.
func nonBenchSlots(positions []string) []string {
	var out []string
	for _, p := range positions {
		if p == "BN" || p == "IR" || p == "TAXI" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// numberedSlots suffixes repeated positions (RB, RB -> RB1, RB2) like the lineup artifact; a lone slot stays bare.
func numberedSlots(positions []string) []string {
	slots := nonBenchSlots(positions)
	counts := map[string]int{}
	for _, s := range slots {
		counts[s]++
	}
	seen := make(map[string]int, len(counts))
	out := make([]string, len(slots))
	for i, s := range slots {
		if counts[s] == 1 {
			out[i] = s
			continue
		}
		seen[s]++
		out[i] = fmt.Sprintf("%s%d", s, seen[s])
	}
	return out
}

func toSet(ids *[]string) map[string]bool {
	if ids == nil {
		return nil
	}
	return toIDSet(*ids)
}
