package sleeper

import (
	"context"
	"testing"
)

func TestGetRoster(t *testing.T) {
	e := testExtension(t)
	got, err := e.getRoster(context.Background(), rosterArgs{})
	if err != nil {
		t.Fatalf("getRoster: %v", err)
	}
	if len(got.Starters) != 9 {
		t.Errorf("starters = %d, want 9", len(got.Starters))
	}
	if len(got.Starters)+len(got.Bench) == 0 {
		t.Error("expected a non-empty roster")
	}
	for _, s := range got.Starters {
		if s.Slot == "?" {
			t.Errorf("starter %+v has an unresolved slot", s)
		}
		if s.Name == "" {
			t.Errorf("starter %+v has no name resolved", s)
		}
	}
	if got.Week != 2 || got.FetchedAtLocal == "" {
		t.Errorf("week/fetched_at_local = %d/%q, want 2/set", got.Week, got.FetchedAtLocal)
	}
	for _, p := range append(append([]slotPlayer{}, got.Starters...), got.Bench...) {
		if p.Game == nil && p.NoGameReason == "" {
			t.Errorf("player %+v has neither a game nor a no_game_reason", p)
		}
		if p.Game != nil && (p.Game.NFLOpponent == "" || p.Game.Kickoff == "" || p.Game.Locked == nil) {
			t.Errorf("player %s game %+v missing opponent/kickoff/locked", p.Name, p.Game)
		}
	}
	if got.FaabLeft != 100-got.FaabUsed {
		t.Errorf("faab_left = %d, want %d", got.FaabLeft, 100-got.FaabUsed)
	}
}

func TestGetRosterByRosterID(t *testing.T) {
	e := testExtension(t)
	got, err := e.getRoster(context.Background(), rosterArgs{RosterID: 1})
	if err != nil {
		t.Fatalf("getRoster: %v", err)
	}
	if got.RosterID != 1 {
		t.Errorf("roster_id = %d, want 1", got.RosterID)
	}
}

func TestGetRosterUnknownUser(t *testing.T) {
	e := testExtension(t)
	if _, err := e.getRoster(context.Background(), rosterArgs{User: "nobody-in-this-league"}); err == nil {
		t.Fatal("expected an error for an unknown user")
	}
}
