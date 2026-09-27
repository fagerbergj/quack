package promptbuilder

import (
	"strings"
	"testing"
	"time"
)

// TestUserZoneClock: a Sunday-night instant that is already Monday in UTC
// renders as Sunday in the user's zone, in both current_date and the prompt line.
func TestUserZoneClock(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 28, 0, 42, 0, 0, time.UTC)
	now = func() time.Time { return fixed }
	SetLocation(chicago)
	t.Cleanup(func() { now = time.Now; SetLocation(nil) })

	if got, want := Now(), "Sunday, September 27, 2026 19:42 CDT (UTC-05:00); UTC 2026-09-28T00:42Z"; got != want {
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
}
