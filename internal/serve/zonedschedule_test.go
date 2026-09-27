package serve

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/fagerbergj/quack/internal/config"
)

// TestZonedSchedule: a bare cron fires at 02:00 in the configured zone; one with its own CRON_TZ keeps it.
func TestZonedSchedule(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for schedule, want := range map[string]string{
		"0 2 * * *":             "2026-09-28T07:00:00Z",
		"CRON_TZ=UTC 0 2 * * *": "2026-09-28T02:00:00Z",
	} {
		sched, err := cron.ParseStandard(zonedSchedule(schedule, chicago))
		if err != nil {
			t.Fatalf("%q: %v", schedule, err)
		}
		if got := sched.Next(from).UTC().Format(time.RFC3339); got != want {
			t.Errorf("%q: next = %s, want %s", schedule, got, want)
		}
	}
	off := ""
	startConsolidationSweep(t.Context(), nil, config.ResolvedMemory{Consolidation: config.ConsolidationConfig{Schedule: &off}}, chicago) // "" disables: no goroutine
}

// TestLogTimezone: a configured zone logs at info; an unknown TZ with no
// configured zone warns instead of silently serving UTC.
func TestLogTimezone(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logTimezone(&config.Config{Timezone: "America/Chicago"})
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "America/Chicago") {
		t.Errorf("configured zone log = %q, want INFO naming America/Chicago", buf.String())
	}

	buf.Reset()
	t.Setenv("TZ", "Mars/Olympus")
	logTimezone(&config.Config{})
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "Mars/Olympus") {
		t.Errorf("bad TZ log = %q, want a WARN naming TZ", buf.String())
	}

	buf.Reset()
	t.Setenv("TZ", "")
	logTimezone(&config.Config{})
	if buf.Len() == 0 {
		t.Error("unset zone logged nothing")
	}
}
