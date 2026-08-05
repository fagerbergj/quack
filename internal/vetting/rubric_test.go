package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanOutputRubricCatchesDeliberation pins clean_output failing visible
// deliberation (self-correction, an abandoned draft, a rewritten snippet),
// not just preamble/trailing narration, across the default rubric and every
// bundle override. Globbed rather than hardcoded, so a new bundle omitting
// the language fails this test instead of silently reopening the gap.
func TestCleanOutputRubricCatchesDeliberation(t *testing.T) {
	rubrics := []string{"../../config/rubric.md"}
	bundleRubrics, err := filepath.Glob("../../agents/*/rubric.md")
	if err != nil {
		t.Fatalf("glob bundle rubrics: %v", err)
	}
	rubrics = append(rubrics, bundleRubrics...)

	var checked int
	for _, path := range rubrics {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rubric := string(raw)

		i := strings.Index(rubric, "`clean_output`")
		if i < 0 {
			continue // rubric doesn't define clean_output - nothing to pin.
		}
		checked++

		t.Run(path, func(t *testing.T) {
			// Isolate the clean_output section so markers can't match a
			// different criterion, then collapse whitespace - the prose wraps
			// mid-phrase, so multi-word markers must match across line breaks.
			section := rubric[i:]
			if j := strings.Index(section[1:], "\n### `"); j >= 0 {
				section = section[:j+1]
			}
			section = strings.Join(strings.Fields(section), " ")

			// A deliberation-laden answer (duplicated/superseded draft) must
			// land in the failing band.
			if !strings.Contains(section, "duplicated") || !strings.Contains(section, "0–3") {
				t.Error("clean_output's failing band does not name a duplicated/superseded draft")
			}
			// A clean answer of equal substance (only the final version
			// present) must land in the passing band.
			if !strings.Contains(section, "only the final version") {
				t.Error("clean_output's passing band does not require only the final version to appear")
			}
			// The description itself must call out mid-body deliberation,
			// not just preamble/trailing narration.
			for _, marker := range []string{"reconsider", "written out more than once"} {
				if !strings.Contains(section, marker) {
					t.Errorf("clean_output description missing deliberation marker %q", marker)
				}
			}
		})
	}

	// Guard the guard: if the glob matched nothing, the loop above would pass
	// vacuously.
	if checked == 0 {
		t.Fatal("no rubric with a clean_output criterion was found")
	}
}

// TestStructuredVerdictRubricCatchesSeverityCoherence pins a live review
// failure: a finding labeled `blocking (security):` on an issue that was
// neither blocking nor security-related, staged alongside an overall APPROVE
// verdict - self-contradictory, and nothing deterministic catches it (the
// maintainer's explicit call: this is a judgment call for the judge to
// reason about, not a regex). Pins that structured_verdict's own text names
// both directions of the contradiction (a blocking/security label under an
// approve verdict, and a request_changes verdict backed by only nits) so a
// live judge is actually told to check it.
func TestStructuredVerdictRubricCatchesSeverityCoherence(t *testing.T) {
	raw, err := os.ReadFile("../../agents/code-reviewer/rubric.md")
	if err != nil {
		t.Fatalf("read code-reviewer rubric: %v", err)
	}
	rubric := string(raw)

	i := strings.Index(rubric, "`structured_verdict`")
	if i < 0 {
		t.Fatal("code-reviewer rubric has no structured_verdict criterion")
	}
	section := rubric[i:]
	if j := strings.Index(section[1:], "\n### `"); j >= 0 {
		section = section[:j+1]
	}
	if j := strings.Index(section, "\n---\n"); j >= 0 {
		section = section[:j]
	}
	section = strings.Join(strings.Fields(section), " ")

	if !strings.Contains(section, "blocking") || !strings.Contains(section, "approve") {
		t.Error("structured_verdict does not name a blocking/security label under an approve verdict as a contradiction")
	}
	if !strings.Contains(section, "request_changes") || !strings.Contains(section, "nit") {
		t.Error("structured_verdict does not name a request_changes verdict backed only by nits as incoherent")
	}
	if !strings.Contains(section, "own severity label") && !strings.Contains(section, "OWN severity label") {
		t.Error("structured_verdict does not tell the judge to cross-check each finding's OWN severity label against the overall verdict")
	}
}

// TestConstructiveActionableRubricScoresCodeBlocks pins the actionability
// extension: a finding that proposes specific code must actually show that
// code (a plain fenced block, NOT a GitHub ```suggestion block - the reviewer
// prompt forbids those for now, staging can't validate their exact-anchor
// discipline yet), while a purely observational finding (a question, a
// naming nit) must be explicitly exempt from that requirement.
func TestConstructiveActionableRubricScoresCodeBlocks(t *testing.T) {
	raw, err := os.ReadFile("../../agents/code-reviewer/rubric.md")
	if err != nil {
		t.Fatalf("read code-reviewer rubric: %v", err)
	}
	rubric := string(raw)

	i := strings.Index(rubric, "`constructive_actionable`")
	if i < 0 {
		t.Fatal("code-reviewer rubric has no constructive_actionable criterion")
	}
	section := rubric[i:]
	if j := strings.Index(section[1:], "\n### `"); j >= 0 {
		section = section[:j+1]
	}
	section = strings.Join(strings.Fields(section), " ")

	if !strings.Contains(section, "fenced code block") {
		t.Error("constructive_actionable does not require a fenced code block for a finding proposing specific code")
	}
	if strings.Contains(section, "suggestion") {
		t.Error("constructive_actionable must not reference GitHub ```suggestion blocks - forbidden for now, plain fenced blocks only")
	}
	if !strings.Contains(section, "PURELY OBSERVATIONAL") && !strings.Contains(section, "purely observational") {
		t.Error("constructive_actionable does not exempt purely observational findings (questions, naming nits) from the code-block requirement")
	}
	if !strings.Contains(section, "NO code block") && !strings.Contains(section, "no code block") {
		t.Error("constructive_actionable does not explicitly say observational findings need no code block")
	}
}
