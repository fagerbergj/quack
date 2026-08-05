package vetting

import (
	"strings"
	"testing"
)

func TestStripLeadingEnvScaffold(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		want   string
	}{
		{
			name:   "scaffolding only becomes empty",
			answer: "<env>\n  Working directory: /workspace/foo\n  entries: .git, README.md\n</env>",
			want:   "",
		},
		{
			name:   "scaffolding then real answer keeps the answer",
			answer: "<env>\n  Working directory: /workspace/foo\n</env>\n\nThe fix is in main.go.",
			want:   "The fix is in main.go.",
		},
		{
			name:   "no scaffolding passes through unchanged",
			answer: "The fix is in main.go.",
			want:   "The fix is in main.go.",
		},
		{
			name:   "an env-like block not at the start is left alone",
			answer: "The fix is in main.go.\n\n<env>not scaffolding</env>",
			want:   "The fix is in main.go.\n\n<env>not scaffolding</env>",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(stripLeadingEnvScaffold(tc.answer))
			if got != tc.want {
				t.Errorf("stripLeadingEnvScaffold(%q) = %q, want %q", tc.answer, got, tc.want)
			}
		})
	}
}
