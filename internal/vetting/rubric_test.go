package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readRubricText: a .yaml rubric renders to markdown; anything else reads as-is.
func readRubricText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(path, ".yaml") {
		doc, err := loadRubricYAML(raw, path)
		if err != nil {
			return "", err
		}
		return renderRubricMarkdown(doc), nil
	}
	return string(raw), nil
}

// TestCleanOutputRubricCatchesDeliberation: clean_output fails visible deliberation, not just
// narration, in the default rubric and every bundle; globbed so a new bundle can't omit it.
func TestCleanOutputRubricCatchesDeliberation(t *testing.T) {
	rubrics := []string{"../../config/rubric.md"}
	bundleRubrics, err := filepath.Glob("../../agents/*/rubric.yaml")
	if err != nil {
		t.Fatalf("glob bundle rubrics: %v", err)
	}
	rubrics = append(rubrics, bundleRubrics...)

	var checked int
	for _, path := range rubrics {
		rubric, err := readRubricText(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		i := strings.Index(rubric, "`clean_output`")
		if i < 0 {
			continue // rubric doesn't define clean_output - nothing to pin.
		}
		checked++

		t.Run(path, func(t *testing.T) {
			// Isolate clean_output so markers can't match another criterion; collapse whitespace
			// since the prose wraps mid-phrase.
			section := rubric[i:]
			if j := strings.Index(section[1:], "\n### `"); j >= 0 {
				section = section[:j+1]
			}
			section = strings.Join(strings.Fields(section), " ")

			// A deliberation-laden answer (duplicated/superseded draft) must
			// land in the failing band.
			if !strings.Contains(section, "duplicated") || !strings.Contains(section, "**0**") {
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

// TestStructuredVerdictRubricCatchesSeverityCoherence: structured_verdict must name both directions
// of a severity/verdict contradiction (blocking label under approve, nits-only request_changes).
func TestStructuredVerdictRubricCatchesSeverityCoherence(t *testing.T) {
	rubric, err := readRubricText("../../.agents/plugins/github/agents/code-reviewer/rubric.yaml")
	if err != nil {
		t.Fatalf("read code-reviewer rubric: %v", err)
	}

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

// TestConstructiveActionableRubricScoresCodeBlocks: a finding proposing code must show it in a
// plain fenced block (not ```suggestion), while observational findings are exempt.
func TestConstructiveActionableRubricScoresCodeBlocks(t *testing.T) {
	rubric, err := readRubricText("../../.agents/plugins/github/agents/code-reviewer/rubric.yaml")
	if err != nil {
		t.Fatalf("read code-reviewer rubric: %v", err)
	}

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
