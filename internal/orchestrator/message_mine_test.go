package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/memory"
)

func TestMinePreferences(t *testing.T) {
	tests := []struct {
		name    string
		message string
		wantN   int
		checks  []func(candidate string) bool
	}{
		{
			name:    "empty message",
			message: "",
			wantN:   0,
		},
		{
			name:    "whitespace only",
			message: "   \n\n  ",
			wantN:   0,
		},
		{
			name:    "no preferences stated",
			message: "Please review this code and tell me if the logic is correct.",
			wantN:   0,
		},
		// --- Verbosity: terminate-style preferences ---
		{
			name:    "keep it terse",
			message: "Keep it terse when you answer",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "terse") && strings.Contains(c, "concise") }},
		},
		{
			name:    "be brief please",
			message: "Be brief please — short answers only.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "brief") || strings.Contains(c, "concise") }},
		},
		{
			name:    "no fluff",
			message: "No fluff — get straight to the point.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "no fluff") }},
		},
		{
			name:    "be detailed",
			message: "Please be detailed and thorough in your response.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "detailed") && strings.Contains(c, "thorough") }},
		},
		// --- PR style ---
		{
			name:    "always open pr",
			message: "Always create a pull request for your changes.",
			wantN:   1,
			checks: []func(string) bool{func(c string) bool {
				return strings.Contains(c, "automatically") && strings.Contains(c, "pull request")
			}},
		},
		{
			name:    "keep on branch no pr",
			message: "Keep work on a branch — do not open a PR.",
			wantN:   1, // only the branch preference; exclude correctly prevents the always-open-PR false positive
			checks: []func(string) bool{
				func(c string) bool { return strings.Contains(c, "branch") },
			},
		},
		{
			name:    "open as draft",
			message: "Please open the PR as a draft.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "draft") }},
		},
		// --- Proceed vs ask ---
		{
			name:    "proceed on best judgment",
			message: "Just do it — proceed on your best judgment. I don't want to be asked every step.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "proceed without asking") }},
		},
		{
			name:    "always ask before acting",
			message: "Confirm with me before doing anything.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "confirm before acting") }},
		},
		// --- Review style ---
		{
			name:    "inline comments",
			message: "I prefer inline review comments instead of summaries.",
			wantN:   1,
			checks: []func(string) bool{func(c string) bool {
				return strings.Contains(c, "inline") && strings.Contains(c, "file-level summaries")
			}},
		},
		{
			name:    "high level overview only",
			message: "Just give me a high level review — no nits.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "high-level") }},
		},
		// --- Communication style ---
		{
			name:    "keep it friendly",
			message: "Keep it friendly and polite in our conversations.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "friendly") && strings.Contains(c, "polite") }},
		},
		// --- Prefixed transient context is stripped ---
		{
			name:    "preference survives context prefix",
			message: "[on my-project#42] keep it terse for future responses too",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "concise") && strings.Contains(c, "terse") }},
		},
		// --- Language preference ---
		{
			name:    "prefer typescript",
			message: "Prefer TypeScript for all code.",
			wantN:   1,
			checks:  []func(string) bool{func(c string) bool { return strings.Contains(c, "TypeScript") }},
		},
		// --- Goal extraction ---
		{
			name:    "goal to learn",
			message: "My goal is to learn Go programming and improve my skills.",
			wantN:   1,
			checks: []func(string) bool{func(c string) bool {
				return strings.Contains(strings.ToLower(c), "learn") && strings.Contains(strings.ToLower(c), "go")
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MinePreferences(tt.message)
			if len(got) != tt.wantN {
				t.Fatalf("MinePreferences() = %d candidates, want %d\n%s", len(got), tt.wantN, formatCandidates(got))
			}
			for i, check := range tt.checks {
				if i >= len(got) {
					break
				}
				content := got[i].Content
				if !check(content) {
					t.Errorf("candidate[%d] content %q failed validity check", i, content)
				}
			}
		})
	}
}

// TestMinePreferencesNoFalsePositives verifies that common non-preference utterances
// produce zero candidates so we don't pollute memory with noise.
func TestMinePreferencesNoFalsePositives(t *testing.T) {
	nop := []string{
		"Please review the code",
		"Can you tell me what files changed?",
		"@quack implement the new auth endpoint",
		"I need help with the database schema",
		"Run the tests and show me output",
		"Just fix this file please",
	}

	for _, msg := range nop {
		got := MinePreferences(msg)
		if len(got) > 0 {
			t.Errorf("no-match message %q produced %d candidates: %s", msg, len(got), formatCandidates(got))
		}
	}
}

// TestMinePreferencesDedup verifies that identical content across patterns
// is deduplicated within the same match set.
func TestMinePreferencesDedupWithinMatch(t *testing.T) {
	msg := "keep it terse be concise" // triggers multiple verbosity words — only one candidate expected
	got := MinePreferences(msg)

	seen := make(map[string]bool)
	for _, c := range got {
		if seen[c.Content] {
			t.Errorf("duplicate content %q produced", c.Content)
		}
		seen[c.Content] = true
	}
}

func formatCandidates(cands []memory.Candidate) string {
	var b strings.Builder
	for i, c := range cands {
		fmt.Fprintf(&b, "  [%d] kind=%s content=%q\n", i, c.Metadata["kind"], c.Content)
	}
	return b.String()
}
