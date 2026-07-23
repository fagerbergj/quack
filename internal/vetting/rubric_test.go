package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCleanOutputRubricCatchesDeliberation pins issue #301: every clean_output
// criterion must fail visible deliberation (self-correction, an abandoned
// draft, a snippet rewritten more than once), not just preamble/trailing
// narration - the gap that let the #252 plan comment (three rewrites of the
// same webhook.go snippet, narrated with "let me reconsider") pass unscored.
// The judge itself is a live LLM call and can't run here, so this pins the
// deterministic rubric text across the default rubric AND every bundle
// override that defines clean_output. It globs rather than hardcoding the list,
// so a NEW bundle rubric that adds clean_output without the deliberation
// language fails this test instead of silently reopening the gap.
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
