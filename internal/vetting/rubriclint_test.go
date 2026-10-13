package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every shipped rubric lints clean; this is the CI gate for rubric edits (go-test runs on agents/ and
// extensions/ changes).
func TestShippedRubricsLintClean(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../../agents/*/rubric.yaml", "../../extensions/*/plugin/agents/*/rubric.yaml", "../../config/rubric*.yaml"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("found %d rubric files, want the shipped set (>= 10): %v", len(files), files)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range LintRubric(raw, f) {
			t.Error(issue)
		}
	}
}

const lintBase = `scale: {min: 0, max: 3, pass: 2}
criteria:
  accurate:
    definition: The answer is right.
    bands:
    - {min: 0, max: 1, meaning: wrong}
    - {min: 2, max: 3, meaning: right}
`

func TestLintRubricFlagsEachProblem(t *testing.T) {
	cases := []struct {
		name, extra, want string
	}{
		{"clean", "", ""},
		{"weight key", "    weight: 2\n", "field weight not found"},
		{"no definition", "  empty:\n    bands: [{min: 0, max: 3, meaning: any}]\n", `"empty": no definition`},
		{"same definition", "  again:\n    definition: the answer is RIGHT.\n    bands: [{min: 0, max: 3, meaning: any}]\n", `"again": same definition as "accurate"`},
		{"unknown deterministic", "  made_up:\n    definition: x\n    deterministic: true\n    fix: y\n", "no check scores"},
		{"judged code-owned name", "  checks_pass:\n    definition: tests pass\n    bands: [{min: 0, max: 3, meaning: any}]\n", "judge's score is dropped"},
		{"no bands", "  vague:\n    definition: it is good\n", "no bands"},
		{"duplicate key", "  accurate:\n    definition: again\n", "already defined"},
		{"judged off the 0-3 scale", "  decimal:\n    definition: graded out of ten\n    scale: {min: 0, max: 10, pass: 7}\n    bands: [{min: 0, max: 10, meaning: any}]\n", "asks for an integer 0-3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := strings.Join(LintRubric([]byte(lintBase+tc.extra), "r.yaml"), "\n")
			if tc.want == "" && issues != "" {
				t.Fatalf("issues = %s, want none", issues)
			}
			if !strings.Contains(issues, tc.want) {
				t.Fatalf("issues = %q, want one containing %q", issues, tc.want)
			}
		})
	}
	// One criterion's problems are all reported, not just the first.
	both := LintRubric([]byte(lintBase+"  decimal:\n    definition: graded out of ten\n    scale: {min: 0, max: 10, pass: 7}\n"), "r.yaml")
	if got := strings.Join(both, "\n"); !strings.Contains(got, "no bands") || !strings.Contains(got, "asks for an integer 0-3") {
		t.Fatalf("issues = %q, want both no bands and the off-scale problem", got)
	}
	// Boot's own validation still runs: a pass mark no band holds is reported too.
	gap := strings.Replace(lintBase, "pass: 2", "pass: 1.5", 1)
	if issues := LintRubric([]byte(gap), "r.yaml"); len(issues) != 1 || !strings.Contains(issues[0], "does not fall inside any band") {
		t.Fatalf("issues = %v, want the band-coverage error", issues)
	}
}
