package sleeper

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// maxFreeAgentHits caps free_agent_hits: a retro needs the headline misses, not every what-if.
const maxFreeAgentHits = 5

type matchupArgs struct {
	LeagueID string `json:"league_id,omitempty"`
	Week     int    `json:"week,omitempty"`
	User     string `json:"user,omitempty"`
	RosterID int    `json:"roster_id,omitempty"`
	TZ       string `json:"tz,omitempty"`
}

type lineupSlot struct {
	Slot       string  `json:"slot"`
	PlayerID   string  `json:"player_id"`
	Name       string  `json:"name"`
	Points     float32 `json:"points"`
	Projection float32 `json:"projection"`
	gameInfo
}

// benchPlayer is a rostered non-starter from a completed week's matchup.
type benchPlayer struct {
	Name       string  `json:"name"`
	Pos        string  `json:"pos"`
	Points     float32 `json:"points"`
	Projection float32 `json:"projection"`
	gameInfo
}

// beatenStarter names the started player a free-agent hit outscored or a swap replaces.
type beatenStarter struct {
	Slot   string  `json:"slot"`
	Name   string  `json:"name"`
	Points float32 `json:"points"`
}

// freeAgentHit is an unrostered player who outscored the weakest starter they could legally replace.
type freeAgentHit struct {
	Name   string        `json:"name"`
	Pos    string        `json:"pos"`
	Team   string        `json:"team"`
	Points float32       `json:"points"`
	Over   beatenStarter `json:"over"`
}

// bestLineupSlot is one slot of a past week's best-by-actual-points lineup, labeled like the lineup artifact.
type bestLineupSlot struct {
	Slot   string  `json:"slot"`
	Name   string  `json:"name"`
	Pos    string  `json:"pos"`
	Points float32 `json:"points"`
}

// bestByProjectionSlot is one slot of a current/future week's best-by-projection lineup.
type bestByProjectionSlot struct {
	Slot       string  `json:"slot"`
	Name       string  `json:"name"`
	Pos        string  `json:"pos"`
	Projection float32 `json:"projection"`
}

// swapPlayer is a best_lineup player who did not actually start.
type swapPlayer struct {
	Name   string  `json:"name"`
	Pos    string  `json:"pos"`
	Points float32 `json:"points"`
}

// swap pairs a best_lineup player with a started player whose slot they could legally fill; matching by
// slot label instead would false-positive on a reordered RB1/RB2.
type swap struct {
	In    swapPlayer    `json:"in"`
	Out   beatenStarter `json:"out"`
	Swing float32       `json:"swing"`
}

type side struct {
	RosterID         int                    `json:"roster_id"`
	Team             string                 `json:"team"`
	Record           string                 `json:"record"`
	Points           float32                `json:"points"`
	Starters         []lineupSlot           `json:"starters"`
	Bench            []benchPlayer          `json:"bench,omitempty"`
	BestPoints       float32                `json:"best_points,omitempty"`
	LeftOnBench      float32                `json:"left_on_bench,omitempty"`
	BestLineup       []bestLineupSlot       `json:"best_lineup,omitempty"`
	Swaps            []swap                 `json:"swaps,omitempty"`
	FreeAgentHits    []freeAgentHit         `json:"free_agent_hits,omitempty"`
	BestByProjection []bestByProjectionSlot `json:"best_by_projection,omitempty"`
}

type matchupResult struct {
	callContext
	MatchupID int  `json:"matchup_id"`
	Me        side `json:"me"`
	Opponent  side `json:"opponent"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) matchupTool() tool.Tool {
	t, _ := functiontool.New[matchupArgs, matchupResult](
		functiontool.Config{
			Name: "sleeper_matchup",
			Description: "Get one fantasy week's matchup (default the current NFL week): both lineups " +
				"with points and projections, and the fantasy opponent's record. `opponent` is the fantasy " +
				"team; each player's NFL opponent is in their game. " + playerGameDoc + " `user`/`roster_id` " +
				"picks my side, falling back to default_user. " + timeDoc,
		},
		func(ctx adkagent.Context, a matchupArgs) (matchupResult, error) { return e.getMatchup(ctx, a) },
	)
	return t
}

func (e *extension) getMatchup(ctx context.Context, a matchupArgs) (matchupResult, error) {
	c := e.newClock(a.TZ)
	leagueID, err := e.resolveLeagueID(a.LeagueID)
	if err != nil {
		return matchupResult{}, err
	}
	league, rosters, users, err := e.leagueRostersUsers(ctx, leagueID)
	if err != nil {
		return matchupResult{}, err
	}
	rosterID, err := e.resolveRosterID(a.RosterID, a.User, rosters, users)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: %w", err)
	}
	season, state, err := e.season(ctx)
	if err != nil {
		return matchupResult{}, err
	}
	week := resolveWeek(a.Week, state)
	matchups, err := e.client.Matchups(ctx, leagueID, week)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: %w", err)
	}
	mine, ok := findMatchup(matchups, rosterID)
	if !ok {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: roster %d has no matchup in week %d", rosterID, week)
	}
	opp, hasOpp := opponent(matchups, mine)
	proj, err := e.client.WeekProjections(ctx, season, week)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: projections: %w", err)
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return matchupResult{}, fmt.Errorf("sleeper_matchup: %w", err)
	}
	cc := newCallContext(c, week, season, state)
	sl := e.playerSlate(ctx, &cc, season, state, c, a.Week == 0)
	names := teamNames(rosters, users)
	result := matchupResult{callContext: cc, Me: buildSide(league, mine, rosters, names, dump, proj, sl)}
	if mine.MatchupId != nil {
		result.MatchupID = *mine.MatchupId
	}
	if hasOpp {
		result.Opponent = buildSide(league, opp, rosters, names, dump, proj, sl)
	}
	if err := e.addWeekFields(ctx, &result.Me, league, state, season, week, mine, matchups, dump, proj, sl); err != nil {
		return matchupResult{}, err
	}
	return result, nil
}

// addWeekFields: a finished week gets the retro fields, a live week the projection baseline.
func (e *extension) addWeekFields(ctx context.Context, me *side, league *sleepergen.League, state *sleepergen.NflState, season string, week int, mine sleepergen.Matchup, matchups []sleepergen.Matchup, dump map[string]sleepergen.Player, proj map[string]sleepergen.StatMap, sl slate) error {
	if !weekIsComplete(week, league, state) {
		me.BestByProjection = bestByProjectionSlots(league.RosterPositions, mine.Players, proj, dump)
		return nil
	}
	stats, err := e.client.WeekStats(ctx, season, week)
	if err != nil {
		return fmt.Errorf("sleeper_matchup: stats: %w", err)
	}
	addRetroFields(me, league, mine, matchups, dump, proj, stats, sl)
	return nil
}

// weekIsComplete: scoring is final before the live week, or once the season is complete.
func weekIsComplete(week int, league *sleepergen.League, state *sleepergen.NflState) bool {
	return league.Status == "complete" || week < state.Week
}

// addRetroFields fills the bench/best-lineup/free-agent data that exists only once scoring is final.
func addRetroFields(me *side, league *sleepergen.League, mine sleepergen.Matchup, matchups []sleepergen.Matchup, dump map[string]sleepergen.Player, proj map[string]sleepergen.StatMap, stats map[string]sleepergen.StatMap, sl slate) {
	me.Bench = benchPlayers(mine, dump, proj, sl)
	assignment := bestLineup(league.RosterPositions, mine.Players, mine.PlayersPoints, dump)
	best := round2(assignment.total)
	me.BestPoints = best
	me.LeftOnBench = round2(best - mine.Points)
	me.BestLineup = bestLineupSlots(league.RosterPositions, assignment, mine.PlayersPoints, dump)
	me.Swaps = buildSwaps(me.Starters, assignment, mine.PlayersPoints, dump)
	me.FreeAgentHits = freeAgentHits(me.Starters, matchups, dump, stats)
}

// buildSwaps pairs each best_lineup player who didn't start with the weakest eligible starter,
// highest in-points first; empty when both lineups hold the same players.
func buildSwaps(starters []lineupSlot, assignment lineupAssignment, points map[string]float32, dump map[string]sleepergen.Player) []swap {
	startedSet := make(map[string]bool, len(starters))
	for _, s := range starters {
		startedSet[s.PlayerID] = true
	}
	bestSet := map[string]bool{}
	for _, pid := range assignment.bySlot {
		if pid != "" {
			bestSet[pid] = true
		}
	}
	return pairSwaps(inPlayers(assignment, startedSet, points, dump), outPlayers(starters, bestSet))
}

// inPlayers is every best_lineup player who didn't start, highest points first.
func inPlayers(assignment lineupAssignment, startedSet map[string]bool, points map[string]float32, dump map[string]sleepergen.Player) []swapPlayer {
	var out []swapPlayer
	for _, pid := range assignment.bySlot {
		if pid == "" || startedSet[pid] {
			continue
		}
		out = append(out, swapPlayer{Name: playerName(dump, pid), Pos: playerPosition(dump, pid), Points: points[pid]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Points > out[j].Points })
	return out
}

// outPlayers is every started player absent from best_lineup.
func outPlayers(starters []lineupSlot, bestSet map[string]bool) []lineupSlot {
	var out []lineupSlot
	for _, s := range starters {
		if s.PlayerID != "" && s.PlayerID != "0" && !bestSet[s.PlayerID] {
			out = append(out, s)
		}
	}
	return out
}

// pairSwaps greedily pairs each in-player with its lowest-scoring unpaired eligible out-player.
func pairSwaps(ins []swapPlayer, outs []lineupSlot) []swap {
	used := make([]bool, len(outs))
	var swaps []swap
	for _, in := range ins {
		idx := weakestEligibleOut(in.Pos, outs, used)
		if idx < 0 {
			continue
		}
		used[idx] = true
		out := outs[idx]
		swaps = append(swaps, swap{In: in, Out: beatenStarter{Slot: out.Slot, Name: out.Name, Points: out.Points}, Swing: round2(in.Points - out.Points)})
	}
	return swaps
}

// weakestEligibleOut is the lowest-scoring unused out slot pos could legally fill, or -1.
func weakestEligibleOut(pos string, outs []lineupSlot, used []bool) int {
	best := -1
	for i, o := range outs {
		if used[i] || (o.Slot != pos && !flexEligible(o.Slot, pos)) {
			continue
		}
		if best < 0 || o.Points < outs[best].Points {
			best = i
		}
	}
	return best
}

// bestLineupSlots labels an assignment with numbered slots, dropping slots the roster couldn't fill.
func bestLineupSlots(rosterPositions []string, a lineupAssignment, points map[string]float32, dump map[string]sleepergen.Player) []bestLineupSlot {
	labels := numberedSlots(rosterPositions)
	out := make([]bestLineupSlot, 0, len(a.bySlot))
	for i, pid := range a.bySlot {
		if pid == "" {
			continue
		}
		out = append(out, bestLineupSlot{Slot: labels[i], Name: playerName(dump, pid), Pos: playerPosition(dump, pid), Points: points[pid]})
	}
	return out
}

// bestByProjectionSlots is the projected best lineup with Out/IR/Doubtful players excluded.
func bestByProjectionSlots(rosterPositions []string, playerIDs []string, proj map[string]sleepergen.StatMap, dump map[string]sleepergen.Player) []bestByProjectionSlot {
	eligible := excludeInjured(playerIDs, dump)
	points := projectionPoints(eligible, proj)
	assignment := bestLineup(rosterPositions, eligible, points, dump)
	labels := numberedSlots(rosterPositions)
	out := make([]bestByProjectionSlot, 0, len(assignment.bySlot))
	for i, pid := range assignment.bySlot {
		if pid == "" {
			continue
		}
		out = append(out, bestByProjectionSlot{Slot: labels[i], Name: playerName(dump, pid), Pos: playerPosition(dump, pid), Projection: points[pid]})
	}
	return out
}

func excludeInjured(playerIDs []string, dump map[string]sleepergen.Player) []string {
	return slices.DeleteFunc(slices.Clone(playerIDs), func(id string) bool { return excludedInjuryStatus(dump[id].InjuryStatus) })
}

func excludedInjuryStatus(status *string) bool {
	if status == nil {
		return false
	}
	switch *status {
	case "Out", "IR", "Doubtful":
		return true
	}
	return false
}

func projectionPoints(playerIDs []string, proj map[string]sleepergen.StatMap) map[string]float32 {
	out := make(map[string]float32, len(playerIDs))
	for _, id := range playerIDs {
		out[id] = proj[id]["pts_ppr"]
	}
	return out
}

// round2 rounds to the 2dp a retro renders, hiding float32 sum noise like 132.05999.
func round2(v float32) float32 {
	return float32(math.Round(float64(v)*100) / 100)
}

// benchPlayers is m.Players minus m.Starters, skipping the "0" empty-slot placeholder.
func benchPlayers(m sleepergen.Matchup, dump map[string]sleepergen.Player, proj map[string]sleepergen.StatMap, sl slate) []benchPlayer {
	started := make(map[string]bool, len(m.Starters))
	for _, pid := range m.Starters {
		started[pid] = true
	}
	var out []benchPlayer
	for _, pid := range m.Players {
		if pid == "0" || started[pid] {
			continue
		}
		out = append(out, benchPlayer{
			Name: playerName(dump, pid), Pos: playerPosition(dump, pid),
			Points: m.PlayersPoints[pid], Projection: proj[pid]["pts_ppr"], gameInfo: sl.gameFor(dump, pid),
		})
	}
	return out
}

// fantasyPosition excludes positions (OL, LS...) the stats dump carries but no roster slot takes.
var fantasyPosition = map[string]bool{"QB": true, "RB": true, "WR": true, "TE": true, "K": true, "DEF": true}

// freeAgentHits lists free-agent hits, worst miss first.
func freeAgentHits(starters []lineupSlot, matchups []sleepergen.Matchup, dump map[string]sleepergen.Player, stats map[string]sleepergen.StatMap) []freeAgentHit {
	rostered := rosteredPlayers(matchups)
	var hits []freeAgentHit
	for pid, s := range stats {
		if rostered[pid] {
			continue
		}
		p, ok := dump[pid]
		if !ok || p.Position == nil || !fantasyPosition[*p.Position] {
			continue
		}
		pts := s["pts_ppr"]
		weakest, ok := weakestEligibleStarter(starters, *p.Position)
		if !ok || pts <= weakest.Points {
			continue
		}
		hits = append(hits, freeAgentHit{
			Name: playerNameOf(p), Pos: *p.Position, Team: strVal(p.Team), Points: pts,
			Over: beatenStarter{Slot: weakest.Slot, Name: weakest.Name, Points: weakest.Points},
		})
	}
	sortFreeAgentHits(hits)
	if len(hits) > maxFreeAgentHits {
		hits = hits[:maxFreeAgentHits]
	}
	return hits
}

// rosteredPlayers unions every roster's Players: a free agent is absent from all of them.
func rosteredPlayers(matchups []sleepergen.Matchup) map[string]bool {
	out := map[string]bool{}
	for _, m := range matchups {
		for _, pid := range m.Players {
			if pid != "0" {
				out[pid] = true
			}
		}
	}
	return out
}

// weakestEligibleStarter is the lowest-scoring starter in a slot pos could legally have filled.
func weakestEligibleStarter(starters []lineupSlot, pos string) (lineupSlot, bool) {
	var weakest lineupSlot
	found := false
	for _, s := range starters {
		if s.Slot != pos && !flexEligible(s.Slot, pos) {
			continue
		}
		if !found || s.Points < weakest.Points {
			weakest, found = s, true
		}
	}
	return weakest, found
}

// sortFreeAgentHits orders by swing descending; the full tiebreak keeps map-iteration order out of the result.
func sortFreeAgentHits(hits []freeAgentHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		si, sj := hits[i].Points-hits[i].Over.Points, hits[j].Points-hits[j].Over.Points
		if si != sj {
			return si > sj
		}
		if hits[i].Points != hits[j].Points {
			return hits[i].Points > hits[j].Points
		}
		return hits[i].Name < hits[j].Name
	})
}

func findMatchup(matchups []sleepergen.Matchup, rosterID int) (sleepergen.Matchup, bool) {
	for _, m := range matchups {
		if m.RosterId == rosterID {
			return m, true
		}
	}
	return sleepergen.Matchup{}, false
}

func opponent(matchups []sleepergen.Matchup, mine sleepergen.Matchup) (sleepergen.Matchup, bool) {
	if mine.MatchupId == nil {
		return sleepergen.Matchup{}, false
	}
	for _, m := range matchups {
		if m.RosterId != mine.RosterId && m.MatchupId != nil && *m.MatchupId == *mine.MatchupId {
			return m, true
		}
	}
	return sleepergen.Matchup{}, false
}

func buildSide(league *sleepergen.League, m sleepergen.Matchup, rosters []sleepergen.Roster, names map[int]string, dump map[string]sleepergen.Player, proj map[string]sleepergen.StatMap, sl slate) side {
	slots := nonBenchSlots(league.RosterPositions)
	starters := make([]lineupSlot, 0, len(m.Starters))
	for i, pid := range m.Starters {
		slot := "?"
		if i < len(slots) {
			slot = slots[i]
		}
		starters = append(starters, lineupSlot{
			Slot: slot, PlayerID: pid, Name: playerName(dump, pid),
			Points: m.PlayersPoints[pid], Projection: proj[pid]["pts_ppr"], gameInfo: sl.gameFor(dump, pid),
		})
	}
	record := ""
	if r, ok := rosterFor(rosters, m.RosterId); ok {
		record = recordString(r.Settings["wins"], r.Settings["losses"], r.Settings["ties"])
	}
	return side{RosterID: m.RosterId, Team: names[m.RosterId], Record: record, Points: m.Points, Starters: starters}
}
