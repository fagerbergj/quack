package sleeper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// TestSnapshotOnceWritesTodaysFile drives the fetch-only path Start's
// ticker calls, against the qa-mock fixtures, and checks the file lands.
func TestSnapshotOnceWritesTodaysFile(t *testing.T) {
	dataDir := t.TempDir()
	e := &extension{
		client: newTestClient(t),
		host:   sdk.Host{DataDir: dataDir},
		cfg:    config{DefaultLeague: testLeague},
	}
	e.snapshotOnce(context.Background())
	dates, err := snapshotDates(dataDir, testLeague)
	if err != nil {
		t.Fatalf("snapshotDates: %v", err)
	}
	if len(dates) != 1 {
		t.Fatalf("snapshot dates = %v, want exactly today's", dates)
	}
	snap, err := readSnapshot(dataDir, testLeague, dates[0])
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	if len(snap.Players) == 0 {
		t.Error("expected rostered players in the snapshot")
	}
}

// TestWriteSnapshotLeavesNoTempFile proves writeSnapshot's temp-file +
// rename leaves only the final .json, no stray .tmp.
func TestWriteSnapshotLeavesNoTempFile(t *testing.T) {
	dataDir := t.TempDir()
	snap := snapshot{Date: "2026-09-18", Players: map[string]playerSnapshot{"a": {DepthChartOrder: 1}}}
	if err := writeSnapshot(dataDir, testLeague, snap); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	entries, err := os.ReadDir(snapshotDir(dataDir, testLeague))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "2026-09-18.json" {
		t.Fatalf("dir entries = %v, want exactly [2026-09-18.json]", entries)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %v, want 0644 (os.CreateTemp defaults to 0600, chmod must correct it)", got)
	}
	got, err := readSnapshot(dataDir, testLeague, "2026-09-18")
	if err != nil || got.Players["a"].DepthChartOrder != 1 {
		t.Errorf("readSnapshot = %+v, %v; want the written snapshot back", got, err)
	}
}

func TestDiffSnapshotsDetectsEachFieldChange(t *testing.T) {
	old := snapshot{
		Date: "2026-09-15",
		Players: map[string]playerSnapshot{
			"11533": {InjuryStatus: "", PracticeDescription: "Full Participation", DepthChartOrder: 1, OwnedPct: 40.0},
			"6797":  {InjuryStatus: "", PracticeDescription: "", DepthChartOrder: 1, OwnedPct: 99.0},
		},
		TrendingAdd: map[string]int{"11533": 100},
	}
	newer := snapshot{
		Date: "2026-09-17",
		Players: map[string]playerSnapshot{
			"11533": {InjuryStatus: "Questionable", PracticeDescription: "Limited Participation", DepthChartOrder: 2, OwnedPct: 55.5},
			"6797":  {InjuryStatus: "", PracticeDescription: "", DepthChartOrder: 1, OwnedPct: 99.0},
		},
		TrendingAdd: map[string]int{"11533": 4200},
	}
	dump, err := newTestClient(t).PlayersDump(context.Background())
	if err != nil {
		t.Fatalf("PlayersDump: %v", err)
	}
	trends := diffSnapshots(old, newer, nil, dump)
	if len(trends) != 1 {
		t.Fatalf("trends = %d, want 1 (only 11533 changed)", len(trends))
	}
	tr := trends[0]
	if tr.PlayerID != "11533" {
		t.Fatalf("player_id = %q, want 11533", tr.PlayerID)
	}
	if tr.Name != "Brandon Aubrey" {
		t.Errorf("name = %q, want Brandon Aubrey (joined from the players dump)", tr.Name)
	}
	if !tr.InjuryStatusChanged || tr.InjuryStatus != "Questionable" {
		t.Errorf("injury_status_changed/injury_status = %v/%q, want true/Questionable", tr.InjuryStatusChanged, tr.InjuryStatus)
	}
	if !tr.PracticeChanged || tr.PracticeDescription != "Limited Participation" {
		t.Errorf("practice_changed/practice_description = %v/%q, want true/Limited Participation", tr.PracticeChanged, tr.PracticeDescription)
	}
	if !tr.DepthChartChanged || tr.DepthChartOrder != 2 {
		t.Errorf("depth_chart_changed/depth_chart_order = %v/%d, want true/2", tr.DepthChartChanged, tr.DepthChartOrder)
	}
	if tr.TrendingAddDelta != 4100 {
		t.Errorf("trending_add_delta = %d, want 4100", tr.TrendingAddDelta)
	}
	if got, want := tr.OwnedPctDelta, float32(15.5); got < want-0.01 || got > want+0.01 {
		t.Errorf("owned_pct_delta = %v, want %v", got, want)
	}
}

func TestDiffSnapshotsFiltersByPlayerIDs(t *testing.T) {
	old := snapshot{Players: map[string]playerSnapshot{"a": {InjuryStatus: ""}, "b": {InjuryStatus: ""}}}
	newer := snapshot{Players: map[string]playerSnapshot{"a": {InjuryStatus: "Out"}, "b": {InjuryStatus: "Out"}}}
	trends := diffSnapshots(old, newer, []string{"a"}, nil)
	if len(trends) != 1 || trends[0].PlayerID != "a" {
		t.Errorf("trends = %+v, want only player a", trends)
	}
}

// TestDiffSnapshotsSortedByPlayerID proves output order is stable
// regardless of Go's randomized map iteration.
func TestDiffSnapshotsSortedByPlayerID(t *testing.T) {
	old := snapshot{Players: map[string]playerSnapshot{}}
	newer := snapshot{Players: map[string]playerSnapshot{
		"z9": {InjuryStatus: "Out"}, "a1": {InjuryStatus: "Out"}, "m5": {InjuryStatus: "Out"},
	}}
	for i := 0; i < 20; i++ {
		trends := diffSnapshots(old, newer, nil, nil)
		if len(trends) != 3 || trends[0].PlayerID != "a1" || trends[1].PlayerID != "m5" || trends[2].PlayerID != "z9" {
			t.Fatalf("trends = %+v, want sorted [a1, m5, z9]", trends)
		}
	}
}

// daysAgo dates a snapshot inside getTrends' window, which is relative to today (withinDays).
func daysAgo(n int) string { return time.Now().UTC().AddDate(0, 0, -n).Format(time.DateOnly) }

// TestGetTrendsOverTwoSyntheticSnapshots writes two on-disk snapshots (the
// shape Start's ticker would have produced) and drives the tool end to end.
func TestGetTrendsOverTwoSyntheticSnapshots(t *testing.T) {
	dataDir := t.TempDir()
	e := &extension{client: newTestClient(t), host: sdk.Host{DataDir: dataDir}, cfg: config{DefaultLeague: testLeague}}
	from, to := daysAgo(6), daysAgo(0)
	old := snapshot{Date: from, Players: map[string]playerSnapshot{"11533": {DepthChartOrder: 1}}}
	newer := snapshot{Date: to, Players: map[string]playerSnapshot{"11533": {DepthChartOrder: 3}}}
	if err := writeSnapshot(dataDir, testLeague, old); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	if err := writeSnapshot(dataDir, testLeague, newer); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	got, err := e.getTrends(context.Background(), trendsArgs{Days: 30})
	if err != nil {
		t.Fatalf("getTrends: %v", err)
	}
	if got.FromDate != from || got.ToDate != to {
		t.Errorf("from/to = %s/%s, want %s/%s", got.FromDate, got.ToDate, from, to)
	}
	if len(got.Trends) != 1 || got.Trends[0].PlayerID != "11533" || !got.Trends[0].DepthChartChanged {
		t.Errorf("trends = %+v, want one depth-chart change for 11533", got.Trends)
	}
	if got.Trends[0].Name != "Brandon Aubrey" {
		t.Errorf("name = %q, want Brandon Aubrey", got.Trends[0].Name)
	}
}

// TestGetTrendsSkipsUnreadableSnapshotWithNote: a corrupt snapshot must not fail the tool call.
func TestGetTrendsSkipsUnreadableSnapshotWithNote(t *testing.T) {
	dataDir := t.TempDir()
	e := &extension{client: newTestClient(t), host: sdk.Host{DataDir: dataDir}, cfg: config{DefaultLeague: testLeague}}
	good := snapshot{Date: daysAgo(6), Players: map[string]playerSnapshot{"11533": {DepthChartOrder: 1}}}
	if err := writeSnapshot(dataDir, testLeague, good); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	corruptPath := filepath.Join(snapshotDir(dataDir, testLeague), daysAgo(0)+".json")
	if err := os.WriteFile(corruptPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write corrupt snapshot: %v", err)
	}
	got, err := e.getTrends(context.Background(), trendsArgs{Days: 30})
	if err != nil {
		t.Fatalf("getTrends must not fail on an unreadable snapshot: %v", err)
	}
	if got.Note == "" {
		t.Error("expected a note explaining the unreadable snapshot")
	}
	if len(got.Trends) != 0 {
		t.Errorf("trends = %+v, want none (only one readable snapshot)", got.Trends)
	}
}

func TestGetTrendsNotesInsufficientHistory(t *testing.T) {
	e := &extension{host: sdk.Host{DataDir: t.TempDir()}, cfg: config{DefaultLeague: testLeague}}
	got, err := e.getTrends(context.Background(), trendsArgs{})
	if err != nil {
		t.Fatalf("getTrends: %v", err)
	}
	if got.Note == "" {
		t.Error("expected a note explaining the missing snapshot history")
	}
}
