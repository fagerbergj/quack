package main

import (
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":         time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		"36h":        now.Add(-36 * time.Hour),
		"2026-10-01": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseSince("soon", now); err == nil {
		t.Error("want error for junk")
	}
	for _, in := range []string{"-7d", "0d", "-36h", "0s"} {
		if _, err := parseSince(in, now); err == nil {
			t.Errorf("parseSince(%q): want non-positive error", in)
		}
	}
}
