package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bundledRubricYAMLs globs every rubric.yaml under agents/ and each plugin's own agents/.
func bundledRubricYAMLs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob("../../agents/*/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pluginMatches, err := filepath.Glob("../../.agents/plugins/*/agents/*/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return append(matches, pluginMatches...)
}

// TestBundledRubricsLoadAndValidate: an invalid shipped rubric is a startup error, so catch it here.
func TestBundledRubricsLoadAndValidate(t *testing.T) {
	matches := bundledRubricYAMLs(t)
	if len(matches) < 9 {
		t.Fatalf("found %d rubric.yaml files, want at least 9 (one per converted agent)", len(matches))
	}
	for _, path := range matches {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := loadRubricYAML(raw, path)
			if err != nil {
				t.Fatalf("load/validate: %v", err)
			}
			if len(doc.Criteria) == 0 {
				t.Errorf("no criteria parsed from %s", path)
			}
		})
	}
}

// TestBundledRubricsNoEmptyBands: every non-deterministic criterion anchors its scale with bands,
// and every integer level of the scale has its own descriptor (no phantom precision).
func TestBundledRubricsNoEmptyBands(t *testing.T) {
	matches := bundledRubricYAMLs(t)
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := loadRubricYAML(raw, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for name, c := range doc.Criteria {
			if len(c.Bands) == 0 {
				t.Errorf("%s: criterion %q has no bands - every scoring level needs a written descriptor", path, name)
				continue
			}
			scale := doc.Scale
			if c.Scale != nil {
				scale = *c.Scale
			}
			for level := scale.Min; level <= scale.Max; level++ {
				if !bandsContain(c.Bands, level) {
					t.Errorf("%s: criterion %q has no band descriptor for level %v of its %v-%v scale", path, name, level, scale.Min, scale.Max)
				}
			}
		}
	}
}

// TestValidateRubricDocCatchesGap: bands that don't cover the whole scale is
// an authoring bug, not a formatting quirk to silently tolerate.
func TestValidateRubricDocCatchesGap(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 10, Pass: 7},
		Criteria: map[string]rubricCriterion{
			"x": {
				Definition: "d",
				Bands: []bandSpec{
					{Min: 0, Max: 3, Meaning: "bad"},
					{Min: 6, Max: 10, Meaning: "good"}, // gap between 3 and 6
				},
			},
		},
	}
	if err := validateRubricDoc(doc); err == nil {
		t.Fatal("want an error for a gap between bands, got nil")
	}
}

// TestValidateRubricDocCatchesOverlap.
func TestValidateRubricDocCatchesOverlap(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 10, Pass: 7},
		Criteria: map[string]rubricCriterion{
			"x": {
				Definition: "d",
				Bands: []bandSpec{
					{Min: 0, Max: 6, Meaning: "bad"},
					{Min: 4, Max: 10, Meaning: "good"}, // overlaps [4,6]
				},
			},
		},
	}
	if err := validateRubricDoc(doc); err == nil {
		t.Fatal("want an error for overlapping bands, got nil")
	}
}

// TestValidateRubricDocRequiresPassInsideBand.
func TestValidateRubricDocRequiresPassInsideBand(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 10, Pass: 11}, // out of range
		Criteria: map[string]rubricCriterion{
			"x": {Definition: "d", Bands: []bandSpec{{Min: 0, Max: 10, Meaning: "m"}}},
		},
	}
	if err := validateRubricDoc(doc); err == nil {
		t.Fatal("want an error for a pass floor outside the scale, got nil")
	}
}

// TestValidateRubricDocRequiresFixForDeterministic.
func TestValidateRubricDocRequiresFixForDeterministic(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 1, Pass: 0.85},
		Criteria: map[string]rubricCriterion{
			"cites_sources": {
				Definition:    "d",
				Deterministic: true,
				Bands:         []bandSpec{{Min: 0, Max: 1, Meaning: "m"}},
				// Fix deliberately omitted.
			},
		},
	}
	if err := validateRubricDoc(doc); err == nil {
		t.Fatal("want an error for a deterministic criterion with no fix, got nil")
	}
}

// TestValidateRubricDocRejectsUnknownAnchorKind.
func TestValidateRubricDocRejectsUnknownAnchorKind(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 10, Pass: 7},
		Criteria: map[string]rubricCriterion{
			"x": {Definition: "d", Bands: []bandSpec{{Min: 0, Max: 10, Meaning: "m"}}, Anchors: []string{"laser"}},
		},
	}
	if err := validateRubricDoc(doc); err == nil {
		t.Fatal("want an error for an unknown anchor kind, got nil")
	}
}

// TestRenderRubricMarkdownWebResearcherGolden pins the judge-facing per-criterion render; the
// general grading preamble belongs to the judge prompt and must not appear.
func TestRenderRubricMarkdownWebResearcherGolden(t *testing.T) {
	raw, err := os.ReadFile("../../agents/web-researcher/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loadRubricYAML(raw, "web-researcher/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderRubricMarkdown(doc)

	for _, want := range []string{
		"### `answers_question`",
		"### `cites_sources`",
		"### `citation_quality`",
		"### `clean_output`",
		"### `grounded`",
		"### `internally_consistent`",
		"**Evaluation steps.**",
		"**Scoring bands.**",
		"- **2** -",
		"- **1** -",
		"- **0** -",
		"Date-awareness",
		"Zero-retrieval handling",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered rubric missing %q\n---\n%s", want, rendered)
		}
	}
	for _, mustNotContain := range []string{
		"How to score (G-Eval)",
		"The 0–10 scale",
		"## Aggregation",
		"flawless on this criterion",
	} {
		if strings.Contains(rendered, mustNotContain) {
			t.Errorf("rendered rubric still carries G-Eval preamble text %q - that moved to judge.go (#941)", mustNotContain)
		}
	}
}

// TestRubricDocSpecsAndFixes: specs and fixes come straight from the doc, no parser in between.
func TestRubricDocSpecsAndFixes(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 10, Pass: 7},
		Criteria: map[string]rubricCriterion{
			"no_fabrication": {Definition: "def", Bands: []bandSpec{{Min: 0, Max: 10, Meaning: "m"}}},
			"cites_sources": {
				Definition: "cs def", Deterministic: true, Fix: "fetch it",
				Scale: &rubricScale{Min: 0, Max: 1, Pass: 0.85},
				Bands: []bandSpec{{Min: 0, Max: 1, Meaning: "m"}},
			},
		},
	}
	specs := rubricDocSpecs(doc)
	if specs["no_fabrication"].Definition != "def" {
		t.Errorf("no_fabrication spec = %+v", specs["no_fabrication"])
	}
	if got := specs["cites_sources"].Scale; got == nil || got.Max != 1 {
		t.Errorf("cites_sources spec scale = %+v, want per-criterion override max=1", got)
	}
	fixes := rubricDocFixes(doc)
	if fixes["cites_sources"] != "fetch it" {
		t.Errorf("fixes[cites_sources] = %q, want %q", fixes["cites_sources"], "fetch it")
	}
	if _, ok := fixes["no_fabrication"]; ok {
		t.Errorf("no_fabrication is not deterministic - should not appear in fixes")
	}
}

// TestGuidanceReachesJudgePromptNotEnvelope: judge-only `guidance` renders into the judge's rubric
// but never reaches rubricDocSpecs, so a worker rejection never surfaces it.
func TestGuidanceReachesJudgePromptNotEnvelope(t *testing.T) {
	doc := rubricDoc{
		Scale: rubricScale{Min: 0, Max: 10, Pass: 7},
		Criteria: map[string]rubricCriterion{
			"no_fabrication": {
				Definition: "Nothing reads as invented.",
				Guidance:   "Recency caveat: your own knowledge is stale, do not flag unfamiliar specifics.",
				Bands:      []bandSpec{{Min: 0, Max: 10, Meaning: "m"}},
			},
		},
	}
	rendered := renderRubricMarkdown(doc)
	if !strings.Contains(rendered, "Recency caveat") {
		t.Errorf("judge prompt render is missing guidance text:\n%s", rendered)
	}

	specs := rubricDocSpecs(doc)
	if strings.Contains(specs["no_fabrication"].Definition, "Recency caveat") {
		t.Errorf("envelope spec leaked judge-only guidance into the worker-facing definition: %+v", specs["no_fabrication"])
	}
	if specs["no_fabrication"].Definition != "Nothing reads as invented." {
		t.Errorf("envelope spec definition = %q, want the short worker-facing sentence only", specs["no_fabrication"].Definition)
	}
}

// TestMemoryAgentRubricRendersNotesAlongsideCriteria: memory-agent's notes still render as
// non-empty text next to its criteria.
func TestMemoryAgentRubricRendersNotesAlongsideCriteria(t *testing.T) {
	raw, err := os.ReadFile("../../agents/memory-agent/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := loadRubricYAML(raw, "memory-agent/rubric.yaml")
	if err != nil {
		t.Fatalf("load/validate: %v", err)
	}
	if len(doc.Criteria) == 0 {
		t.Fatal("expected memory-agent to carry criteria, got 0")
	}
	rendered := renderRubricMarkdown(doc)
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("rendered text is empty - the notes-only content must still reach the caller (guidance prose, judge prompt)")
	}
	if !strings.Contains(rendered, "Candidate quality bar") {
		t.Errorf("rendered rubric missing its notes content:\n%s", rendered)
	}
}

// TestEnvelopeFromCriteriaLessVerdictIsEmpty: a verdict with no criteria yields empty
// Passing/Deterministic/Judge arrays, not an error.
func TestEnvelopeFromCriteriaLessVerdictIsEmpty(t *testing.T) {
	env := buildEnvelope(verdict{}, 0.7, 1)
	if len(env.Passing) != 0 || len(env.DeterministicFailures) != 0 || len(env.JudgeFailures) != 0 {
		t.Errorf("envelope from a criteria-less verdict should have all-empty arrays, got %+v", env)
	}
}

// TestDeterministicCriterionRendersWithoutBands: the judge's rubric names a
// code-owned criterion as decided elsewhere and gives it nothing to score against.
func TestDeterministicCriterionRendersWithoutBands(t *testing.T) {
	doc := rubricDoc{Criteria: map[string]rubricCriterion{
		"cites_sources": {Definition: "cs def", Deterministic: true, Fix: "fetch it",
			Steps: []string{"count the links"}, Bands: []bandSpec{{Min: 0, Max: 1, Meaning: "backed"}}},
	}}
	rendered := renderRubricMarkdown(doc)
	for _, want := range []string{"cs def", "Code-owned"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered rubric missing %q:\n%s", want, rendered)
		}
	}
	for _, leak := range []string{"count the links", "Scoring bands"} {
		if strings.Contains(rendered, leak) {
			t.Errorf("rendered rubric gives the judge %q to score a code-owned criterion:\n%s", leak, rendered)
		}
	}
}
