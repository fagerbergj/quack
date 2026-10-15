package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// weatherCheckedCriterion: every unlocked outdoor game in a lineup row marked `close` has a weather call
// for its venue and kickoff. Computed only for a rubric that declares it.
const weatherCheckedCriterion = "weather_checked"

// weatherCoordTolerance: degrees a call may sit from the venue, since Open-Meteo snaps to its grid.
const weatherCoordTolerance = 0.1

// dataCall: one completed data-tool call, unrendered.
type dataCall struct {
	name       string
	args, resp map[string]any
}

// closeRow: the fields of a lineup starters/bench row this check reads.
type closeRow struct {
	Close  bool `json:"close"`
	Player struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"player"`
}

// gameNeed: one unlocked outdoor game a close call involves. Zero lat/lon: the venue had no coordinates.
type gameNeed struct {
	venue    string
	kickoff  time.Time
	lat, lon float64
	players  []string
}

func weatherCheckedCriterionScore(ctx context.Context, deliverable string, act workerActivity, cfg Config) (criterionScore, bool) {
	if !declaresCodeOwned(cfg, weatherCheckedCriterion) {
		return criterionScore{}, false
	}
	rows, ok := closeRows(ctx, act, recordReader(cfg))
	if !ok {
		return criterionScore{}, false
	}
	needs := outdoorCloseGames(rows, playerGames(act.dataCalls))
	if len(needs) == 0 {
		return criterionScore{Score: 1, Reason: "deterministic: no lineup row marked close is in an unlocked outdoor game - nothing to check"}, true
	}
	// A weather error (e.g. past the forecast horizon) counts once the answer or artifact says so.
	low := strings.ToLower(deliverable)
	acknowledged := strings.Contains(low, "weather") || strings.Contains(low, "forecast")
	c := criterionScore{}
	var missing []string
	for _, n := range needs {
		covered := weatherCovers(n, act.dataCalls, acknowledged)
		c.Evidence = append(c.Evidence, evidenceItem{Ref: n.venue, Score: boolScore(covered)})
		if covered {
			c.Score++
			continue
		}
		missing = append(missing, fmt.Sprintf("%s at %s (%s)", n.venue, n.kickoff.Format(time.RFC3339), strings.Join(n.players, ", ")))
	}
	c.Reason = fmt.Sprintf("deterministic: %d of %d unlocked outdoor games in a close call have a weather call for their venue and kickoff", int(c.Score), len(needs))
	if len(missing) > 0 {
		c.Reason += "; unchecked: " + strings.Join(missing, "; ")
	}
	c.Score /= float64(len(needs))
	return c, true
}

func boolScore(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// closeRows: the close-marked rows of the lineup artifacts this node wrote; false when none was readable.
func closeRows(ctx context.Context, act workerActivity, load PageLoader) ([]closeRow, bool) {
	if load == nil {
		return nil, false
	}
	var rows []closeRow
	found := false
	for _, id := range act.artifactsWritten {
		body, _, ok, err := load.Latest(ctx, id)
		var doc struct{ Starters, Bench []closeRow }
		if err != nil || !ok || json.Unmarshal(body, &doc) != nil || len(doc.Starters) == 0 {
			continue
		}
		found = true
		for _, r := range append(doc.Starters, doc.Bench...) {
			if r.Close {
				rows = append(rows, r)
			}
		}
	}
	return rows, found
}

// playerGames: each player_id's `game` object from the session's data-tool replies; a later reply wins.
func playerGames(calls []dataCall) map[string]map[string]any {
	games := map[string]map[string]any{}
	for _, c := range calls {
		collectPlayerGames(games, c.resp)
	}
	return games
}

func collectPlayerGames(dst map[string]map[string]any, v any) {
	switch x := v.(type) {
	case map[string]any:
		if id, ok := x["player_id"].(string); ok {
			if g, ok := x["game"].(map[string]any); ok {
				dst[id] = g
			}
		}
		for _, c := range x {
			collectPlayerGames(dst, c)
		}
	case []any:
		for _, c := range x {
			collectPlayerGames(dst, c)
		}
	}
}

// outdoorCloseGames: the distinct games of rows whose game is explicitly unlocked and outdoor; locked
// absent means possibly locked, and a player with no game (bye) has nothing to check.
func outdoorCloseGames(rows []closeRow, games map[string]map[string]any) []*gameNeed {
	byID := map[string]*gameNeed{}
	var needs []*gameNeed
	for _, r := range rows {
		g := games[r.Player.ID]
		if g == nil || g["locked"] != false || g["roof"] != "outdoor" {
			continue
		}
		id, _ := g["game_id"].(string)
		n, ok := byID[id]
		if !ok {
			n = newGameNeed(g)
			byID[id] = n
			needs = append(needs, n)
		}
		n.players = append(n.players, r.Player.Name)
	}
	return needs
}

func newGameNeed(g map[string]any) *gameNeed {
	n := &gameNeed{}
	n.venue, _ = g["venue"].(string)
	n.lat, _ = g["latitude"].(float64)
	n.lon, _ = g["longitude"].(float64)
	if k, ok := g["kickoff"].(string); ok {
		n.kickoff, _ = time.Parse(time.RFC3339, k)
	}
	return n
}

// weatherCovers: a weather call for n's kickoff and venue that succeeded, or errored and was acknowledged.
func weatherCovers(n *gameNeed, calls []dataCall, acknowledged bool) bool {
	for _, c := range calls {
		if c.name != "weather" || !callMatchesGame(c, n) {
			continue
		}
		if _, errored := c.resp["error"]; !errored || acknowledged {
			return true
		}
	}
	return false
}

func callMatchesGame(c dataCall, n *gameNeed) bool {
	if !n.kickoff.IsZero() {
		s, _ := c.args["kickoff"].(string)
		k, err := time.Parse(time.RFC3339, s)
		if err != nil || !k.Equal(n.kickoff) {
			return false
		}
	}
	if n.lat == 0 && n.lon == 0 {
		return true
	}
	lat, lon, ok := callCoords(c)
	return ok && math.Abs(lat-n.lat) <= weatherCoordTolerance && math.Abs(lon-n.lon) <= weatherCoordTolerance
}

// callCoords: the call's own latitude/longitude, else the ones a location lookup resolved to.
func callCoords(c dataCall) (float64, float64, bool) {
	for _, m := range []map[string]any{c.args, c.resp} {
		lat, latOK := m["latitude"].(float64)
		lon, lonOK := m["longitude"].(float64)
		if latOK && lonOK {
			return lat, lon, true
		}
	}
	return 0, 0, false
}
