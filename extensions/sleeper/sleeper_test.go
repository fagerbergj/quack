package sleeper

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

func TestFactoryRejectsUnknownKey(t *testing.T) {
	_, err := factory(sdk.Host{}, []byte("default_user: jf\nbogus_key: 1\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown config key, got nil")
	}
}

func TestFactorySnapshotsValidation(t *testing.T) {
	for _, tc := range []struct {
		snapshots string
		wantErr   bool
	}{
		{"", false},
		{"off", false},
		{"daily", false},
		{"weekly", true},
	} {
		raw := []byte("snapshots: " + tc.snapshots + "\n")
		if tc.snapshots == "" {
			raw = nil
		}
		_, err := factory(sdk.Host{}, raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("snapshots=%q: err=%v, wantErr=%v", tc.snapshots, err, tc.wantErr)
		}
	}
}

// TestUIDescriptor pins the nav entry quack's rail renders: an inline SVG
// icon (not a name quack has to ship/recognize), themed via currentColor.
func TestUIDescriptor(t *testing.T) {
	e := &extension{}
	got := e.UI()
	if got.Href != "/sleeper/" {
		t.Errorf("Href = %q, want /sleeper/", got.Href)
	}
	if got.Title != "Sleeper" {
		t.Errorf("Title = %q, want Sleeper", got.Title)
	}
	if !strings.HasPrefix(got.Icon, "<svg") {
		t.Errorf("Icon = %q, want it to start with <svg", got.Icon)
	}
	if !strings.Contains(got.Icon, `fill="currentColor"`) {
		t.Error(`Icon must carry fill="currentColor" so it follows the rail's theme`)
	}
}

// wantToolCount is every sleeper_* tool this slice registers.
const wantToolCount = 12

// TestFactoryIsSideEffectFree calls factory with a zero-value Host - factory
// (and building the tool list) must never invoke any of its nil fields.
func TestFactoryIsSideEffectFree(t *testing.T) {
	ext, err := factory(sdk.Host{}, []byte("default_user: jf\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if got := len(ext.Tools()); got != wantToolCount {
		t.Errorf("Tools() returned %d tools, want %d", got, wantToolCount)
	}
}

// TestStartNoopWithoutDailySnapshots covers both guards: snapshots not
// "daily", and no default_league to snapshot.
func TestStartNoopWithoutDailySnapshots(t *testing.T) {
	for _, cfg := range []config{
		{Snapshots: "off", DefaultLeague: testLeague},
		{Snapshots: "daily", DefaultLeague: ""},
	} {
		e := &extension{host: sdk.Host{}, cfg: cfg}
		if err := e.Start(context.Background()); err != nil {
			t.Errorf("Start(%+v) = %v, want nil", cfg, err)
		}
	}
}

func TestFactoryTimezone(t *testing.T) {
	ext, err := factory(sdk.Host{}, []byte("timezone: America/Chicago\n"))
	if err != nil {
		t.Fatalf("timezone America/Chicago: %v", err)
	}
	if loc := ext.(*extension).loc; loc == nil || loc.String() != "America/Chicago" {
		t.Errorf("loc = %v, want America/Chicago", loc)
	}
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		host *time.Location
		raw  string
		want string // "" = nil, i.e. time.Local
	}{
		{"host only", denver, "", "America/Denver"},
		{"config over host", denver, "timezone: America/Chicago\n", "America/Chicago"},
		{"fixed-offset host", time.FixedZone("EST", -5*3600), "", ""},
	} {
		ext, err := factory(sdk.Host{Location: tc.host, Log: slog.New(slog.DiscardHandler)}, []byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if loc := ext.(*extension).loc; (loc == nil && tc.want != "") || (loc != nil && loc.String() != tc.want) {
			t.Errorf("%s: loc = %v, want %q", tc.name, loc, tc.want)
		}
	}
	for _, bad := range []string{"CDT", "EST", "America/Chicgo"} {
		if _, err := factory(sdk.Host{}, []byte("timezone: "+bad+"\n")); err == nil || !strings.Contains(err.Error(), "timezone") {
			t.Errorf("timezone %q: err = %v, want a timezone error", bad, err)
		}
	}
}
