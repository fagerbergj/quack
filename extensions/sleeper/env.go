package sleeper

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/fagerbergj/quack/extensions/sleeper/sleepergen"
)

// playerEnv is a player's own NFL situation; teamInjuries carries the teammates around him.
type playerEnv struct {
	NFLTeam  string     `json:"nfl_team,omitempty"`
	Injury   string     `json:"injury_status,omitempty"`
	Practice string     `json:"practice,omitempty"`
	Depth    string     `json:"depth,omitempty"`
	Ahead    []teammate `json:"ahead,omitempty"`
}

// teammate is a depth-chart neighbor: the starter ahead of a handcuff, or a hurt QB/WR around a pass catcher.
type teammate struct {
	Name   string `json:"name"`
	Pos    string `json:"pos"`
	Depth  string `json:"depth,omitempty"`
	Injury string `json:"injury_status,omitempty"`
}

// teamDepth is each NFL team's depth-charted players, indexed once per call so per-row lookups stay cheap.
type teamDepth map[string][]sleepergen.Player

// keyDepth is how deep a hurt teammate still shapes a player's role: starters and their direct backups.
const keyDepth = 2

var offensePosition = map[string]bool{"QB": true, "RB": true, "WR": true, "TE": true}

func newTeamDepth(dump map[string]sleepergen.Player) teamDepth {
	td := teamDepth{}
	for _, p := range dump {
		if p.Team != nil && p.DepthChartPosition != nil && p.DepthChartOrder != nil {
			td[*p.Team] = append(td[*p.Team], p)
		}
	}
	for _, ps := range td {
		slices.SortFunc(ps, func(a, b sleepergen.Player) int {
			return cmp.Or(cmp.Compare(*a.DepthChartPosition, *b.DepthChartPosition), cmp.Compare(*a.DepthChartOrder, *b.DepthChartOrder))
		})
	}
	return td
}

func (td teamDepth) envOf(p sleepergen.Player) playerEnv {
	env := playerEnv{NFLTeam: strVal(p.Team), Injury: strVal(p.InjuryStatus), Practice: strVal(p.PracticeParticipation), Depth: depthLabel(p)}
	if p.DepthChartPosition == nil || p.DepthChartOrder == nil {
		return env
	}
	for _, t := range td[env.NFLTeam] {
		if *t.DepthChartPosition == *p.DepthChartPosition && *t.DepthChartOrder < *p.DepthChartOrder {
			env.Ahead = append(env.Ahead, teammateOf(t))
		}
	}
	return env
}

// injuries lists each named team's hurt QB/RB/WR/TE starters and backups, the context a hurt teammate gives.
func (td teamDepth) injuries(teams []string) map[string][]teammate {
	out := map[string][]teammate{}
	for _, team := range teams {
		for _, t := range td[team] {
			if strVal(t.InjuryStatus) != "" && *t.DepthChartOrder <= keyDepth && offensePosition[strVal(t.Position)] {
				out[team] = append(out[team], teammateOf(t))
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func teammateOf(p sleepergen.Player) teammate {
	return teammate{Name: playerNameOf(p), Pos: strVal(p.Position), Depth: depthLabel(p), Injury: strVal(p.InjuryStatus)}
}

// depthLabel is Sleeper's depth slot and order, e.g. RB2 or LWR1.
func depthLabel(p sleepergen.Player) string {
	if p.DepthChartPosition == nil || p.DepthChartOrder == nil {
		return ""
	}
	return fmt.Sprintf("%s%d", *p.DepthChartPosition, *p.DepthChartOrder)
}

// teamsOf is the distinct NFL teams of rows' envs, in first-seen order.
func teamsOf(envs ...playerEnv) []string {
	var out []string
	for _, e := range envs {
		if e.NFLTeam != "" && !slices.Contains(out, e.NFLTeam) {
			out = append(out, e.NFLTeam)
		}
	}
	return out
}

// addEnv gives an unplayed week's starters their situation today; a finished week's would be hindsight.
func addEnv(dump map[string]sleepergen.Player, sides ...*side) map[string][]teammate {
	td := newTeamDepth(dump)
	var envs []playerEnv
	for _, s := range sides {
		for i := range s.Starters {
			s.Starters[i].playerEnv = td.envOf(dump[s.Starters[i].PlayerID])
			envs = append(envs, s.Starters[i].playerEnv)
		}
	}
	return td.injuries(teamsOf(envs...))
}
