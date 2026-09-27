package promptbuilder

import (
	"context"
	"strings"
	"testing"
	"time"
)

func useClock(t *testing.T, loc *time.Location, at *time.Time) {
	t.Helper()
	now = func() time.Time { return *at }
	SetLocation(loc)
	t.Cleanup(func() { now = time.Now; SetLocation(nil) })
}

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestUserZoneClock: a Sunday-night instant that is already Monday in UTC
// renders as Sunday in the user's zone, in both current_date and the prompt line.
func TestUserZoneClock(t *testing.T) {
	at := time.Date(2026, 9, 28, 0, 42, 0, 0, time.UTC)
	useClock(t, chicago(t), &at)

	if got, want := Now(), "Sunday, September 27, 2026 19:42 CDT (UTC-05:00, America/Chicago); UTC 2026-09-28T00:42Z"; got != want {
		t.Errorf("Now() = %q, want %q", got, want)
	}
	line := "Today is Sunday, 2026-09-27 in the user's time zone (CDT, UTC-05:00)."
	if out := Agent("a", "d", nil, nil, false, "", "", ""); !strings.Contains(out, line) {
		t.Errorf("Agent() missing %q:\n%s", line, out)
	}

	SetLocation(time.UTC)
	if got, want := today(), "Monday, 2026-09-28 in the user's time zone (UTC, UTC+00:00)"; got != want {
		t.Errorf("today() in UTC = %q, want %q", got, want)
	}
	SetLocation(nil)
	if got := Now(); strings.Contains(got, "Local") {
		t.Errorf("Now() with no configured zone = %q, want no zone name", got)
	}
	if got := today(); !strings.Contains(got, "the server's time zone") {
		t.Errorf("today() with no configured zone = %q, want the server's time zone", got)
	}
}

// TestNowDSTNotice: within a week of a DST switch, current_date names the
// next offset, so a kickoff after the switch converts correctly.
func TestNowDSTNotice(t *testing.T) {
	at := time.Date(2026, 10, 31, 17, 0, 0, 0, time.UTC)
	useClock(t, chicago(t), &at)
	want := "Saturday, October 31, 2026 12:00 CDT (UTC-05:00, America/Chicago); UTC 2026-10-31T17:00Z; switches to CST (UTC-06:00) at 2026-11-01T07:00Z"
	if got := Now(); got != want {
		t.Errorf("Now() = %q, want %q", got, want)
	}
	at = time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	if got := Now(); strings.Contains(got, "switches") {
		t.Errorf("Now() 12 days out = %q, want no switch notice", got)
	}
}

// TestCacheByDayRollover: the cached prompt rebuilds at local midnight and at a
// DST offset change, but not within one local day.
func TestCacheByDayRollover(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to time.Time
		rebuild  bool
	}{
		{"same local day", time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC), time.Date(2026, 9, 28, 4, 0, 0, 0, time.UTC), false},
		{"local midnight", time.Date(2026, 9, 28, 4, 59, 0, 0, time.UTC), time.Date(2026, 9, 28, 5, 1, 0, 0, time.UTC), true},
		{"DST fall back", time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := tc.from
			useClock(t, chicago(t), &at)
			builds := 0
			cached := CacheByDay(nil, func(context.Context) string { builds++; return "" })
			cached(context.Background())
			at = tc.to
			cached(context.Background())
			if got := builds == 2; got != tc.rebuild {
				t.Errorf("builds = %d, want rebuild=%v", builds, tc.rebuild)
			}
		})
	}
}
