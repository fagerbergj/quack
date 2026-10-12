package sleeper

import (
	"context"
	"fmt"
	"time"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// waiverTypeName follows Sleeper's documented settings.waiver_type enum.
func waiverTypeName(v int) string {
	switch v {
	case 0:
		return "rolling"
	case 1:
		return "reverse_standings"
	case 2:
		return "faab"
	default:
		return "unknown"
	}
}

type userArgs struct {
	Username string `json:"username,omitempty"`
	UserID   string `json:"user_id,omitempty"`
}

type userLeagueSummary struct {
	LeagueID string `json:"league_id"`
	Name     string `json:"name"`
	Season   string `json:"season"`
	Status   string `json:"status"`
}

type userResult struct {
	UserID      string              `json:"user_id"`
	DisplayName string              `json:"display_name"`
	Leagues     []userLeagueSummary `json:"leagues"`
	FetchedAt   string              `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) userTool() tool.Tool {
	t, _ := functiontool.New[userArgs, userResult](
		functiontool.Config{
			Name: "sleeper_user",
			Description: "Look up a Sleeper user (by username or user_id, falling back to the configured " +
				"default_user) and the leagues they're in this season. Returns {user_id, display_name, leagues}.",
		},
		func(ctx adkagent.Context, a userArgs) (userResult, error) { return e.getUser(ctx, a) },
	)
	return t
}

func (e *extension) getUser(ctx context.Context, a userArgs) (userResult, error) {
	id := a.UserID
	if id == "" {
		id = a.Username
	}
	id, err := e.resolveUserIdentifier(id)
	if err != nil {
		return userResult{}, err
	}
	u, err := e.client.User(ctx, id)
	if err != nil {
		return userResult{}, fmt.Errorf("sleeper_user: %w", err)
	}
	season, _, err := e.season(ctx)
	if err != nil {
		return userResult{}, err
	}
	leagues, err := e.client.UserLeagues(ctx, u.UserId, season)
	if err != nil {
		return userResult{}, fmt.Errorf("sleeper_user: leagues: %w", err)
	}
	out := make([]userLeagueSummary, len(leagues))
	for i, l := range leagues {
		out[i] = userLeagueSummary{LeagueID: l.LeagueId, Name: l.Name, Season: l.Season, Status: l.Status}
	}
	return userResult{UserID: u.UserId, DisplayName: strVal(u.DisplayName), Leagues: out, FetchedAt: nowRFC3339()}, nil
}

type leagueArgs struct {
	LeagueID string `json:"league_id,omitempty"`
}

type leagueResult struct {
	LeagueID         string             `json:"league_id"`
	Name             string             `json:"name"`
	Season           string             `json:"season"`
	Week             int                `json:"week"`
	RosterPositions  []string           `json:"roster_positions"`
	ReserveSlots     int                `json:"reserve_slots"`
	TaxiSlots        int                `json:"taxi_slots"`
	PlayoffTeams     int                `json:"playoff_teams"`
	PlayoffWeekStart int                `json:"playoff_week_start"`
	WaiverType       string             `json:"waiver_type"`
	WaiverBudget     int                `json:"waiver_budget"`
	ScoringSettings  map[string]float32 `json:"scoring_settings"`
	FetchedAt        string             `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) leagueTool() tool.Tool {
	t, _ := functiontool.New[leagueArgs, leagueResult](
		functiontool.Config{
			Name: "sleeper_league",
			Description: "Get a league's settings: scoring, roster_positions, reserve/taxi slot counts, " +
				"playoff config, waiver type and budget, season, and the current week. Falls back to the " +
				"configured default_league when league_id is omitted.",
		},
		func(ctx adkagent.Context, a leagueArgs) (leagueResult, error) { return e.getLeague(ctx, a) },
	)
	return t
}

func (e *extension) getLeague(ctx context.Context, a leagueArgs) (leagueResult, error) {
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return leagueResult{}, err
	}
	l, err := e.client.League(ctx, leagueID)
	if err != nil {
		return leagueResult{}, fmt.Errorf("sleeper_league: %w", err)
	}
	state, err := e.client.State(ctx)
	if err != nil {
		return leagueResult{}, fmt.Errorf("sleeper_league: state: %w", err)
	}
	return leagueResult{
		LeagueID:         l.LeagueId,
		Name:             l.Name,
		Season:           l.Season,
		Week:             state.Week,
		RosterPositions:  l.RosterPositions,
		ReserveSlots:     l.Settings["reserve_slots"],
		TaxiSlots:        l.Settings["taxi_slots"],
		PlayoffTeams:     l.Settings["playoff_teams"],
		PlayoffWeekStart: l.Settings["playoff_week_start"],
		WaiverType:       waiverTypeName(l.Settings["waiver_type"]),
		WaiverBudget:     l.Settings["waiver_budget"],
		ScoringSettings:  l.ScoringSettings,
		FetchedAt:        nowRFC3339(),
	}, nil
}

type scheduleArgs struct {
	Week int    `json:"week,omitempty"`
	TZ   string `json:"tz,omitempty"`
}

type scheduleGame struct {
	GameID       string `json:"game_id"`
	Home         string `json:"home"`
	Away         string `json:"away"`
	Date         string `json:"date,omitempty"`
	Kickoff      string `json:"kickoff,omitempty"`
	KickoffLocal string `json:"kickoff_local,omitempty"`
	KickoffTBD   bool   `json:"kickoff_tbd,omitempty"`
	Locked       *bool  `json:"locked,omitempty"`
	Status       string `json:"status,omitempty"`
	Venue        string `json:"venue,omitempty"`
	City         string `json:"city,omitempty"`
	Roof         string `json:"roof,omitempty"`
}

type scheduleResult struct {
	callContext
	Games []scheduleGame `json:"games"`
}

// Description text shared by every tool that returns games.
const (
	timeDoc = "kickoff/fetched_at are UTC; show the user kickoff_local/fetched_at_local, already in their " +
		"zone - never convert UTC yourself. `tz` (IANA name) only overrides that zone; normally omit it. " +
		"week/current_week/week_note say which NFL week the data covers (default: the current week)."
	lockDoc = "locked: true once kickoff has passed as of fetched_at or the game is in_game/complete; absent " +
		"= unknown, treat as possibly locked. Give kickoff times only for locked=false games. kickoff_tbd: " +
		"flex game, time not set yet. Canceled/postponed games carry no kickoff."
	playerGameDoc = "Each player's own NFL game this week is already joined in code: game {game_id, " +
		"nfl_opponent, is_home, kickoff, kickoff_local, kickoff_tbd, locked, status, roof, venue}, or " +
		"instead bye=true or no_game_reason. Use it as-is; never take a kickoff or opponent from another game. " + lockDoc
)

func (e *extension) scheduleTool() tool.Tool {
	t, _ := functiontool.New[scheduleArgs, scheduleResult](
		functiontool.Config{
			Name: "sleeper_schedule",
			Description: "Get the NFL schedule for a week (default the current week). Per game: status " +
				"(pre_game, in_game, complete, canceled, postponed), date (US Eastern calendar date), " +
				"kickoff, kickoff_local, locked, venue, city, roof (outdoor/dome/retractable_dome). " +
				lockDoc + " " + timeDoc + " A rostered player's game is already joined by " +
				"sleeper_roster/sleeper_matchup/sleeper_player.",
		},
		func(ctx adkagent.Context, a scheduleArgs) (scheduleResult, error) { return e.getSchedule(ctx, a) },
	)
	return t
}

func (e *extension) getSchedule(ctx context.Context, a scheduleArgs) (scheduleResult, error) {
	c := e.newClock(a.TZ)
	season, state, err := e.season(ctx)
	if err != nil {
		return scheduleResult{}, err
	}
	week := resolveWeek(a.Week, state)
	sl, err := e.weekSlate(ctx, season, week, c)
	if err != nil {
		return scheduleResult{}, fmt.Errorf("sleeper_schedule: %w", err)
	}
	res := scheduleResult{callContext: newCallContext(c, week, season, state), Games: sl.games}
	res.ScheduleNote = sl.note
	res.noteWeekDone(sl, a.Week == 0)
	return res, nil
}

// noteWeekDone flags the gap between Monday night and Sleeper's Tuesday
// week rollover, when the defaulted "current" week is already over.
func (cc *callContext) noteWeekDone(sl slate, defaulted bool) {
	if !defaulted || len(sl.games) == 0 {
		return
	}
	for _, g := range sl.games {
		if g.Status != "complete" && !voidGame(g.Status) {
			return
		}
	}
	cc.addWeekNote(fmt.Sprintf("every week-%d game is over; Sleeper has not rolled over to week %d yet", cc.Week, cc.Week+1))
}

// slate is one week's games with kickoff/lock joined, indexed by team code
// so per-player lookups never guess which game a player is in.
type slate struct {
	games  []scheduleGame
	byTeam map[string]scheduleGame
	teams  map[string]bool // every team code on the season's schedule
	note   string
	noGame string // set when players get no join at all, e.g. schedule unavailable
}

func (e *extension) weekSlate(ctx context.Context, season string, week int, c clock) (slate, error) {
	games, err := e.client.Schedule(ctx, season)
	if err != nil {
		return slate{}, err
	}
	scores, err := e.client.WeekScores(ctx, season, week)
	if err != nil {
		e.logWarn("sleeper: scores fetch failed", "season", season, "week", week, "error", err)
	}
	byID := make(map[string]*sleepergen.GameScore, len(scores))
	for i := range scores {
		byID[scores[i].GameId] = &scores[i]
	}
	sl := slate{byTeam: map[string]scheduleGame{}, teams: map[string]bool{}}
	for _, g := range games {
		sl.teams[g.Home], sl.teams[g.Away] = true, true
		if g.Week != week {
			continue
		}
		sg := withKickoff(scheduleGame{GameID: g.GameId, Home: g.Home, Away: g.Away, Date: strVal(g.Date), Status: strVal(g.Status)}, byID[g.GameId], c)
		sl.games = append(sl.games, sg)
		for _, team := range []string{sg.Home, sg.Away} {
			if prev, ok := sl.byTeam[team]; !ok || voidGame(prev.Status) {
				sl.byTeam[team] = sg
			}
		}
	}
	if len(scores) == 0 && len(sl.games) > 0 {
		sl.note = "kickoff times unavailable from Sleeper this call; locked reflects game status only"
	}
	return sl, nil
}

// playerSlate is the slate for per-player joins. It never fails the tool,
// and joins only the live regular-season week: the dump holds today's teams.
func (e *extension) playerSlate(ctx context.Context, cc *callContext, season string, state *sleepergen.NflState, c clock, defaulted bool) slate {
	switch {
	case state.SeasonType != "regular":
		return slate{noGame: fmt.Sprintf("NFL %s season", state.SeasonType)}
	case season != state.Season || cc.Week != state.Week:
		return slate{noGame: "games are joined only for the current NFL week"}
	}
	sl, err := e.weekSlate(ctx, season, cc.Week, c)
	if err != nil {
		e.logWarn("sleeper: schedule fetch failed", "season", season, "error", err)
		cc.ScheduleNote = "schedule unavailable"
		return slate{noGame: "schedule unavailable"}
	}
	cc.ScheduleNote = sl.note
	cc.noteWeekDone(sl, defaulted)
	return sl
}

func voidGame(status string) bool { return status == "canceled" || status == "postponed" }

// gameInfo is a player's own game for the slate's week, or why there is none.
type gameInfo struct {
	Game         *playerGame `json:"game,omitempty"`
	Bye          bool        `json:"bye,omitempty"`
	NoGameReason string      `json:"no_game_reason,omitempty"`
}

// playerGame is the per-player slice of a scheduleGame; city/date stay in sleeper_schedule.
type playerGame struct {
	GameID       string `json:"game_id"`
	NFLOpponent  string `json:"nfl_opponent"`
	IsHome       bool   `json:"is_home"`
	Kickoff      string `json:"kickoff,omitempty"`
	KickoffLocal string `json:"kickoff_local,omitempty"`
	KickoffTBD   bool   `json:"kickoff_tbd,omitempty"`
	Locked       *bool  `json:"locked,omitempty"`
	Status       string `json:"status,omitempty"`
	Roof         string `json:"roof,omitempty"`
	Venue        string `json:"venue,omitempty"`
}

func (sl slate) gameFor(dump map[string]sleepergen.Player, playerID string) gameInfo {
	if playerID == "" || playerID == "0" {
		return gameInfo{NoGameReason: "empty lineup slot"}
	}
	if sl.noGame != "" {
		return gameInfo{NoGameReason: sl.noGame}
	}
	team := nflTeam(dump, playerID)
	switch {
	case team == "":
		return gameInfo{NoGameReason: "no NFL team (free agent or unknown player)"}
	case !sl.teams[team]:
		return gameInfo{NoGameReason: "unknown team code " + team}
	case len(sl.games) == 0:
		return gameInfo{NoGameReason: "no NFL games scheduled this week"}
	}
	g, ok := sl.byTeam[team]
	switch {
	case !ok:
		return gameInfo{Bye: true, NoGameReason: team + " has no game this week (bye)"}
	case voidGame(g.Status):
		return gameInfo{NoGameReason: fmt.Sprintf("%s's game this week is %s", team, g.Status)}
	}
	pg := playerGame{GameID: g.GameID, NFLOpponent: g.Home, IsHome: g.Home == team, Kickoff: g.Kickoff,
		KickoffLocal: g.KickoffLocal, KickoffTBD: g.KickoffTBD, Locked: g.Locked, Status: g.Status, Roof: g.Roof, Venue: g.Venue}
	if pg.IsHome {
		pg.NFLOpponent = g.Away
	}
	return gameInfo{Game: &pg}
}

// nflTeam is a player's NFL team code; a DEF unit's player_id is its team code.
func nflTeam(dump map[string]sleepergen.Player, playerID string) string {
	p, ok := dump[playerID]
	if !ok {
		return ""
	}
	if team := strVal(p.Team); team != "" {
		return team
	}
	if strVal(p.Position) == "DEF" {
		return playerID
	}
	return ""
}

// withKickoff joins on game_id, never on teams, so a missing score leaves
// kickoff empty rather than borrowing another game's time.
func withKickoff(g scheduleGame, s *sleepergen.GameScore, c clock) scheduleGame {
	statusLocked := g.Status == "in_game" || g.Status == "complete"
	if statusLocked {
		g.Locked = &statusLocked
	}
	if s == nil {
		return g
	}
	if sd := s.Metadata.StadiumDetails; sd != nil {
		g.Venue, g.City, g.Roof = strVal(sd.Name), strVal(sd.City), strVal(sd.Type)
	}
	switch strVal(s.Metadata.Status) {
	case "flex-schedule":
		g.KickoffTBD = true
		return g
	case "postponed", "cancelled": // postponed games keep their original, now meaningless, start_time
		return g
	}
	if voidGame(g.Status) || s.StartTime <= 0 {
		return g
	}
	k := time.UnixMilli(s.StartTime).UTC()
	g.Kickoff, g.KickoffLocal = k.Format(time.RFC3339), c.local(k)
	// The schedule's status (2m cache) can be newer than a 6h-cached start_time.
	locked := statusLocked || !k.After(c.now)
	g.Locked = &locked
	return g
}
