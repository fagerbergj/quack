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
