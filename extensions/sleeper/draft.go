// sleeper_draft: draft state, order, picks with ADP, who's on the clock, and best available by position.
package sleeper

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// noADPSentinel: Sleeper reports 999/1000 adp_dd_ppr for no real consensus,
// not a literal 999th-overall pick.
const noADPSentinel = 999

// offensePositions bounds best-available grouping to draftable skill spots.
var offensePositions = []string{"QB", "RB", "WR", "TE", "K", "DEF"}

// bestAvailablePerPosition is enough names for a tier read without flooding the result.
const bestAvailablePerPosition = 5

// adpDeltaThreshold: |pick_no - adp| at or past this is "value" or "reach"; render.js's renderDraftBoard
// uses the same band for its fell/reach classes.
const adpDeltaThreshold = 8

type draftArgs struct {
	DraftID  string `json:"draft_id,omitempty"`
	LeagueID string `json:"league_id,omitempty"`
}

type draftSlot struct {
	Slot     int    `json:"slot"`
	RosterID int    `json:"roster_id"`
	Team     string `json:"team"`
}

type draftPick struct {
	PickNo   int     `json:"pick_no"`
	Round    int     `json:"round"`
	Slot     int     `json:"slot"`
	RosterID int     `json:"roster_id"`
	Team     string  `json:"team"`
	PlayerID string  `json:"player_id"`
	Name     string  `json:"name"`
	Position string  `json:"position"`
	ADP      float32 `json:"adp,omitempty"`
	NoADP    bool    `json:"no_adp,omitempty"`
	// ADPDelta/ADPVerdict are computed here so the agent never recomputes them; a sign error inverts
	// steal and reach.
	ADPDelta   *float32 `json:"adp_delta,omitempty"`
	ADPVerdict string   `json:"adp_verdict,omitempty"`
}

type draftResult struct {
	DraftID       string                 `json:"draft_id"`
	Status        string                 `json:"status"`
	Type          string                 `json:"type"`
	Season        string                 `json:"season"`
	Order         []draftSlot            `json:"order"`
	Picks         []draftPick            `json:"picks"`
	OnClock       *draftSlot             `json:"on_clock,omitempty"`
	BestAvailable map[string][]playerRef `json:"best_available,omitempty"`
	FetchedAt     string                 `json:"fetched_at"`
}

//nolint:dupl // functiontool wrapper boilerplate: each tool differs only in name/description/handler
func (e *extension) draftTool() tool.Tool {
	t, _ := functiontool.New[draftArgs, draftResult](
		functiontool.Config{
			Name: "sleeper_draft",
			Description: "Get a draft's state: order/slot per team, every pick so far with player and ADP " +
				"(999/1000 = no ADP), who's on the clock, and best available by position. Pass `draft_id` " +
				"directly or `league_id` to use that league's draft.",
		},
		func(ctx adkagent.Context, a draftArgs) (draftResult, error) { return e.getDraft(ctx, a) },
	)
	return t
}

func (e *extension) getDraft(ctx context.Context, a draftArgs) (draftResult, error) {
	draftID, leagueID, err := e.resolveDraftID(ctx, a.DraftID, a.LeagueID)
	if err != nil {
		return draftResult{}, err
	}
	d, err := e.client.Draft(ctx, draftID)
	if err != nil {
		return draftResult{}, fmt.Errorf("sleeper_draft: %w", err)
	}
	picks, err := e.client.DraftPicks(ctx, draftID)
	if err != nil {
		return draftResult{}, fmt.Errorf("sleeper_draft: picks: %w", err)
	}
	rosters, users, err := e.rostersAndUsers(ctx, leagueID)
	if err != nil {
		return draftResult{}, err
	}
	dump, err := e.client.PlayersDump(ctx)
	if err != nil {
		return draftResult{}, fmt.Errorf("sleeper_draft: %w", err)
	}
	proj, err := e.draftProjections(ctx, d.Season)
	if err != nil {
		return draftResult{}, err
	}
	names := teamNames(rosters, users)
	result := draftResult{
		DraftID: d.DraftId, Status: d.Status, Type: d.Type, Season: d.Season,
		Order:     draftOrder(d, names),
		Picks:     draftPicksOut(picks, names, proj),
		OnClock:   onClock(d, e.draftRounds(ctx, d), len(picks), names),
		FetchedAt: nowRFC3339(),
	}
	if d.Status == "drafting" {
		result.BestAvailable = bestAvailable(dump, picks, proj)
	}
	return result, nil
}

// draftRounds falls back to the league's starter-slot count when settings.rounds is unset.
func (e *extension) draftRounds(ctx context.Context, d *sleepergen.Draft) int {
	if r := d.Settings["rounds"]; r > 0 {
		return r
	}
	league, err := e.client.League(ctx, d.LeagueId)
	if err != nil {
		return 0
	}
	return len(nonBenchSlots(league.RosterPositions))
}

// resolveDraftID returns draft_id as given, else the league's draft; the league id feeds team names.
func (e *extension) resolveDraftID(ctx context.Context, draftID, leagueID string) (string, string, error) {
	if draftID != "" {
		return draftID, leagueID, nil
	}
	leagueID, err := e.resolveLeagueID(leagueID)
	if err != nil {
		return "", "", err
	}
	drafts, err := e.client.LeagueDrafts(ctx, leagueID)
	if err != nil {
		return "", "", fmt.Errorf("sleeper_draft: league drafts: %w", err)
	}
	if len(drafts) == 0 {
		return "", "", fmt.Errorf("sleeper_draft: league %s has no drafts", leagueID)
	}
	league, err := e.client.League(ctx, leagueID)
	if err != nil {
		return "", "", fmt.Errorf("sleeper_draft: league: %w", err)
	}
	return pickDraft(drafts, league.Season).DraftId, leagueID, nil
}

// pickDraft prefers the draft matching season (a re-draft leaves several), else the latest start_time.
func pickDraft(drafts []sleepergen.Draft, season string) sleepergen.Draft {
	for _, d := range drafts {
		if d.Season == season {
			return d
		}
	}
	best := drafts[0]
	for _, d := range drafts[1:] {
		if intVal(d.StartTime) > intVal(best.StartTime) {
			best = d
		}
	}
	return best
}

func (e *extension) rostersAndUsers(ctx context.Context, leagueID string) ([]sleepergen.Roster, []sleepergen.LeagueUser, error) {
	if leagueID == "" {
		return nil, nil, nil
	}
	rosters, err := e.client.Rosters(ctx, leagueID)
	if err != nil {
		return nil, nil, fmt.Errorf("sleeper_draft: rosters: %w", err)
	}
	users, err := e.client.LeagueUsers(ctx, leagueID)
	if err != nil {
		return nil, nil, fmt.Errorf("sleeper_draft: league users: %w", err)
	}
	return rosters, users, nil
}

// draftProjections reads the current week's projections: every week carries the season-long adp_dd_ppr.
func (e *extension) draftProjections(ctx context.Context, season string) (map[string]sleepergen.StatMap, error) {
	state, err := e.client.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("sleeper_draft: state: %w", err)
	}
	week := state.Week
	if week < 1 {
		week = 1
	}
	proj, err := e.client.WeekProjections(ctx, season, week)
	if err != nil {
		return nil, fmt.Errorf("sleeper_draft: projections: %w", err)
	}
	return proj, nil
}

func draftOrder(d *sleepergen.Draft, names map[int]string) []draftSlot {
	if d.SlotToRosterId == nil {
		return nil
	}
	out := make([]draftSlot, 0, len(*d.SlotToRosterId))
	for slotStr, rosterID := range *d.SlotToRosterId {
		slot, err := strconv.Atoi(slotStr)
		if err != nil {
			continue
		}
		out = append(out, draftSlot{Slot: slot, RosterID: rosterID, Team: names[rosterID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

func draftPicksOut(picks []sleepergen.DraftPick, names map[int]string, proj map[string]sleepergen.StatMap) []draftPick {
	out := make([]draftPick, len(picks))
	for i, p := range picks {
		adp := proj[p.PlayerId]["adp_dd_ppr"]
		noADP := adp >= noADPSentinel
		out[i] = draftPick{
			PickNo: p.PickNo, Round: p.Round, Slot: p.DraftSlot, RosterID: p.RosterId, Team: names[p.RosterId],
			PlayerID: p.PlayerId, Name: pickPlayerName(p), Position: pickPosition(p),
			ADP: adp, NoADP: noADP,
		}
		out[i].ADPDelta, out[i].ADPVerdict = adpDeltaAndVerdict(p.PickNo, adp, noADP)
	}
	return out
}

// adpDeltaAndVerdict: delta is picks fallen past ADP (negative = reached); nil/"" without a real ADP.
func adpDeltaAndVerdict(pickNo int, adp float32, noADP bool) (*float32, string) {
	if noADP || adp <= 0 {
		return nil, ""
	}
	delta := math.Round(float64(float32(pickNo)-adp)*10) / 10
	delta32 := float32(delta)
	verdict := "fair"
	switch {
	case delta >= adpDeltaThreshold:
		verdict = "value"
	case delta <= -adpDeltaThreshold:
		verdict = "reach"
	}
	return &delta32, verdict
}

func pickPlayerName(p sleepergen.DraftPick) string {
	if p.Metadata == nil {
		return p.PlayerId
	}
	m := *p.Metadata
	name := (m["first_name"] + " " + m["last_name"])
	if name == " " {
		return p.PlayerId
	}
	return name
}

func pickPosition(p sleepergen.DraftPick) string {
	if p.Metadata == nil {
		return ""
	}
	return (*p.Metadata)["position"]
}

// onClock computes the next pick from snake-draft math; nil unless drafting with picks left.
func onClock(d *sleepergen.Draft, rounds, picksMade int, names map[int]string) *draftSlot {
	if d.Status != "drafting" || d.SlotToRosterId == nil {
		return nil
	}
	teams := d.Settings["teams"]
	if teams == 0 {
		teams = len(*d.SlotToRosterId)
	}
	if teams == 0 || rounds == 0 || picksMade >= teams*rounds {
		return nil
	}
	pickNo := picksMade + 1
	round := (pickNo-1)/teams + 1
	posInRound := (pickNo - 1) % teams
	slot := posInRound + 1
	if d.Type == "snake" && round%2 == 0 {
		slot = teams - posInRound
	}
	rosterID := (*d.SlotToRosterId)[strconv.Itoa(slot)]
	return &draftSlot{Slot: slot, RosterID: rosterID, Team: names[rosterID]}
}

func bestAvailable(dump map[string]sleepergen.Player, picks []sleepergen.DraftPick, proj map[string]sleepergen.StatMap) map[string][]playerRef {
	picked := make(map[string]bool, len(picks))
	for _, p := range picks {
		picked[p.PlayerId] = true
	}
	out := make(map[string][]playerRef, len(offensePositions))
	for _, pos := range offensePositions {
		var ids []string
		for pid, p := range dump {
			if picked[pid] || strVal(p.Position) != pos {
				continue
			}
			ids = append(ids, pid)
		}
		ids = sortedByProjection(ids, proj)
		if len(ids) > bestAvailablePerPosition {
			ids = ids[:bestAvailablePerPosition]
		}
		refs := make([]playerRef, len(ids))
		for i, id := range ids {
			refs[i] = playerRef{PlayerID: id, Name: playerName(dump, id)}
		}
		out[pos] = refs
	}
	return out
}
