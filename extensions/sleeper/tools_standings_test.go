package sleeper

import (
	"context"
	"testing"
)

func TestGetStandings(t *testing.T) {
	e := testExtension(t)
	got, err := e.getStandings(context.Background(), standingsArgs{})
	if err != nil {
		t.Fatalf("getStandings: %v", err)
	}
	if len(got.Standings) != 10 {
		t.Errorf("standings rows = %d, want 10 (num_teams)", len(got.Standings))
	}
	for i, row := range got.Standings {
		if row.Rank != i+1 {
			t.Errorf("row %d: rank = %d, want %d", i, row.Rank, i+1)
		}
		if row.InPlayoff != (row.Rank <= 6) {
			t.Errorf("row %d: in_playoff_line = %v, want rank<=6", i, row.InPlayoff)
		}
	}
	for i := 1; i < len(got.Standings); i++ {
		prev, cur := got.Standings[i-1], got.Standings[i]
		if cur.Wins > prev.Wins || (cur.Wins == prev.Wins && cur.Fpts > prev.Fpts) {
			t.Errorf("standings not sorted: row %d (%v) should not outrank row %d (%v)", i, cur, i-1, prev)
		}
	}
}
