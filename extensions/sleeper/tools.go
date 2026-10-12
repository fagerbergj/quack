// Shared tool plumbing: resolving user/league/week from args or config, and naming players/teams.
package sleeper

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the `tz` argument must resolve even in an image without /usr/share/zoneinfo

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// localLayout is the *_local display format, e.g. "Sun Sep 27 3:05 PM CDT".
const localLayout = "Mon Jan 2 3:04 PM MST"

// clock is one tool call's single "now" plus the zone its *_local fields render in.
type clock struct {
	now    time.Time
	loc    *time.Location
	tzNote string
}

// loadZone accepts only region-style IANA names (or UTC): EST and friends load as fixed offsets, an
// hour off during DST.
func loadZone(name string) (*time.Location, error) {
	if name != "UTC" && !strings.Contains(name, "/") {
		return nil, fmt.Errorf("%q is not a region-style IANA zone name like America/Chicago", name)
	}
	return time.LoadLocation(name)
}

// newClock picks the display zone: the `tz` override, else configured, else host, else process zone.
// A bad override falls back with a tz_note.
func (e *extension) newClock(tz string) clock {
	c := clock{now: time.Now(), loc: time.Local}
	if e.loc != nil {
		c.loc = e.loc
	}
	if tz = strings.TrimSpace(tz); tz != "" {
		loc, err := loadZone(tz)
		if err != nil {
			c.tzNote = fmt.Sprintf("tz ignored (%v); times are in %s", err, c.loc)
			return c
		}
		c.loc = loc
	}
	return c
}

func (c clock) local(t time.Time) string { return t.In(c.loc).Format(localLayout) }

// callContext heads every week-scoped result: the week covered versus the live week, and when it was read.
type callContext struct {
	Week           int    `json:"week"`
	CurrentWeek    int    `json:"current_week,omitempty"`
	WeekNote       string `json:"week_note,omitempty"`
	ScheduleNote   string `json:"schedule_note,omitempty"`
	FetchedAt      string `json:"fetched_at"`
	FetchedAtLocal string `json:"fetched_at_local"`
	TZNote         string `json:"tz_note,omitempty"`
}

func newCallContext(c clock, week int, season string, state *sleepergen.NflState) callContext {
	cc := callContext{Week: week, FetchedAt: c.now.UTC().Format(time.RFC3339), FetchedAtLocal: c.local(c.now), TZNote: c.tzNote}
	switch {
	case season != state.Season:
		cc.WeekNote = fmt.Sprintf("season %s week %d, not the live NFL season (%s)", season, week, state.Season)
	case week != state.Week:
		cc.CurrentWeek = state.Week
		cc.WeekNote = fmt.Sprintf("week %d is NOT the current NFL week (%d)", week, state.Week)
	default:
		cc.CurrentWeek = state.Week
	}
	if state.SeasonType != "regular" {
		cc.addWeekNote(fmt.Sprintf("NFL %s season; the regular-season schedule does not apply", state.SeasonType))
	}
	return cc
}

func (cc *callContext) addWeekNote(note string) {
	if cc.WeekNote != "" {
		note = cc.WeekNote + "; " + note
	}
	cc.WeekNote = note
}

// nowRFC3339 stamps fetched_at so a judge can tell how stale the (possibly cached) data is.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// resolveLeagueID falls back to the extension's configured default league.
func (e *extension) resolveLeagueID(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if e.cfg.DefaultLeague != "" {
		return e.cfg.DefaultLeague, nil
	}
	return "", fmt.Errorf("league_id is required (no default_league configured)")
}

// resolveUserIdentifier falls back to the extension's configured default user.
func (e *extension) resolveUserIdentifier(arg string) (string, error) {
	if arg != "" {
		return arg, nil
	}
	if e.cfg.DefaultUser != "" {
		return e.cfg.DefaultUser, nil
	}
	return "", fmt.Errorf("user is required (no default_user configured)")
}

// season returns the live season from /state/nfl.
func (e *extension) season(ctx context.Context) (string, *sleepergen.NflState, error) {
	state, err := e.client.State(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("sleeper: state: %w", err)
	}
	return state.Season, state, nil
}

// resolveWeek returns arg if positive, else the live week.
func resolveWeek(arg int, state *sleepergen.NflState) int {
	if arg > 0 {
		return arg
	}
	return state.Week
}

// resolveRosterID returns rosterID if set, else the roster owned by user (default_user if empty).
func (e *extension) resolveRosterID(rosterID int, user string, rosters []sleepergen.Roster, users []sleepergen.LeagueUser) (int, error) {
	if rosterID != 0 {
		return rosterID, nil
	}
	uid, err := e.resolveUserIdentifier(user)
	if err != nil {
		return 0, err
	}
	id, ok := rosterIDForUser(rosters, users, uid)
	if !ok {
		return 0, fmt.Errorf("no roster found for %q", uid)
	}
	return id, nil
}

// rosterIDForUser finds the roster owned by a user_id, else by a display or team name.
func rosterIDForUser(rosters []sleepergen.Roster, users []sleepergen.LeagueUser, userOrName string) (int, bool) {
	for _, r := range rosters {
		if r.OwnerId != nil && *r.OwnerId == userOrName {
			return r.RosterId, true
		}
	}
	needle := strings.ToLower(userOrName)
	for _, u := range users {
		if strings.ToLower(u.DisplayName) != needle && strings.ToLower(teamName(u)) != needle {
			continue
		}
		for _, r := range rosters {
			if r.OwnerId != nil && *r.OwnerId == u.UserId {
				return r.RosterId, true
			}
		}
	}
	return 0, false
}

// teamName prefers the user's league team_name over their display name.
func teamName(u sleepergen.LeagueUser) string {
	if u.Metadata != nil {
		if n, ok := (*u.Metadata)["team_name"]; ok {
			if trimmed := strings.TrimSpace(n); trimmed != "" {
				return trimmed
			}
		}
	}
	return ownerName(u)
}

// ownerName trims a display name: some carry trailing spaces, and every surface must agree.
func ownerName(u sleepergen.LeagueUser) string {
	return strings.TrimSpace(u.DisplayName)
}

// teamNames maps roster_id to team name, joining rosters' owner_id to league users.
func teamNames(rosters []sleepergen.Roster, users []sleepergen.LeagueUser) map[int]string {
	byUser := make(map[string]string, len(users))
	for _, u := range users {
		byUser[u.UserId] = teamName(u)
	}
	out := make(map[int]string, len(rosters))
	for _, r := range rosters {
		name := fmt.Sprintf("roster %d", r.RosterId)
		if r.OwnerId != nil {
			if n, ok := byUser[*r.OwnerId]; ok {
				name = n
			}
		}
		out[r.RosterId] = name
	}
	return out
}

func playerName(dump map[string]sleepergen.Player, playerID string) string {
	p, ok := dump[playerID]
	if !ok {
		return playerID
	}
	return playerNameOf(p)
}

// playerPosition is "" for an unknown player and "DEF" for a team-defense id like "CAR".
func playerPosition(dump map[string]sleepergen.Player, playerID string) string {
	return strVal(dump[playerID].Position)
}

func rosterFor(rosters []sleepergen.Roster, rosterID int) (sleepergen.Roster, bool) {
	for _, r := range rosters {
		if r.RosterId == rosterID {
			return r, true
		}
	}
	return sleepergen.Roster{}, false
}

// sortedByProjection ranks ids by pts_ppr projection, the one figure every Sleeper projection carries.
func sortedByProjection(ids []string, proj map[string]sleepergen.StatMap) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		return proj[out[i]]["pts_ppr"] > proj[out[j]]["pts_ppr"]
	})
	return out
}
