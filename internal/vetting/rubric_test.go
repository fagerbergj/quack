package vetting

import (
	"os"
	"strings"
	"testing"
)

// TestCleanOutputRubricCatchesDeliberation pins issue #301: clean_output must
// fail visible deliberation (self-correction, an abandoned draft, a snippet
// rewritten more than once), not just preamble/trailing narration — the gap
// that let the #252 plan comment (three rewrites of the same webhook.go
// snippet, narrated with "let me reconsider") pass unscored. The judge itself
// is an LLM call and can't run here, so this pins the deterministic rubric
// text both directions: the failing band explicitly names a duplicated draft,
// and the passing band explicitly requires only the final version to appear.
func TestCleanOutputRubricCatchesDeliberation(t *testing.T) {
	cases := []struct {
		name string
		load func() (string, error)
	}{
		{"default", func() (string, error) {
			raw, err := os.ReadFile("../../config/rubric.md")
			return string(raw), err
		}},
		{"synthesizer override", func() (string, error) {
			return LoadBundleRubric("../../agents/synthesizer")
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rubric, err := c.load()
			if err != nil {
				t.Fatalf("load rubric: %v", err)
			}
			if rubric == "" {
				t.Fatal("rubric is empty")
			}

			i := strings.Index(rubric, "`clean_output`")
			if i < 0 {
				t.Fatal("rubric has no clean_output criterion")
			}
			// Isolate the clean_output section so markers can't match a
			// different criterion.
			section := rubric[i:]
			if j := strings.Index(section[1:], "\n### `"); j >= 0 {
				section = section[:j+1]
			}
			// Prose in the rubric wraps mid-phrase; collapse whitespace so
			// multi-word markers match regardless of line breaks.
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
}
