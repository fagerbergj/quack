package sleeper

import (
	"testing"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

// TestTeamNameTrims: Sleeper names can carry trailing spaces; every surface goes through teamName/ownerName.
func TestTeamNameTrims(t *testing.T) {
	meta := map[string]string{"team_name": "Brown Tuddies Likely "}
	u := sleepergen.LeagueUser{DisplayName: "raw display ", Metadata: &meta}
	if got := teamName(u); got != "Brown Tuddies Likely" {
		t.Errorf("teamName = %q, want trimmed team_name", got)
	}
	if got := ownerName(u); got != "raw display" {
		t.Errorf("ownerName = %q, want trimmed display name", got)
	}
}

func TestTeamNameFallsBackToTrimmedDisplayName(t *testing.T) {
	u := sleepergen.LeagueUser{DisplayName: "Roster Owner "}
	if got := teamName(u); got != "Roster Owner" {
		t.Errorf("teamName = %q, want the trimmed display name (no team_name set)", got)
	}
}

// TestTeamNameBlankMetadataFallsBack: a team_name that trims to "" must
// fall through to the display name, not return empty.
func TestTeamNameBlankMetadataFallsBack(t *testing.T) {
	meta := map[string]string{"team_name": "   "}
	u := sleepergen.LeagueUser{DisplayName: "Roster Owner", Metadata: &meta}
	if got := teamName(u); got != "Roster Owner" {
		t.Errorf("teamName = %q, want the display name when team_name is blank", got)
	}
}

func TestRecordString(t *testing.T) {
	if got := recordString(3, 4, 0); got != "3-4" {
		t.Errorf("recordString(3,4,0) = %q, want 3-4 (ties omitted when zero)", got)
	}
	if got := recordString(3, 4, 1); got != "3-4-1" {
		t.Errorf("recordString(3,4,1) = %q, want 3-4-1", got)
	}
}
