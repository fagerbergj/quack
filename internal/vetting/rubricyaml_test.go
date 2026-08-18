package vetting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBundledRubricsLoadAndValidate loads every shipped agents/*/rubric.yaml
// and validates it - #941: a rubric that fails to validate is a startup
// error, so this catches an authoring mistake in any of the nine converted
// rubrics before it ever reaches a running judge.
func TestBundledRubricsLoadAndValidate(t *testing.T) {
	matches, err := filepath.Glob("../../agents/*/rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
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
			if path == "../../agents/memory-agent/rubric.yaml" {
				if len(doc.Criteria) != 0 {
					t.Errorf("memory-agent rubric is prose guidance, not a scored rubric - want 0 criteria, got %d", len(doc.Criteria))
				}
				return
			}
			if len(doc.Criteria) == 0 {
				t.Errorf("no criteria parsed from %s", path)
			}
		})
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

// TestRenderRubricMarkdownWebResearcherGolden pins the judge-facing render:
// per-criterion headers/definition/steps/bands stay in the same shape the
// judge prompt has always carried, and the deleted rubric.md's G-Eval
// preamble ("How to score", the 0-10 scale walkthrough, "Aggregation") is
// gone - that's now judge.go content, not rubric content (#941 redirect).
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
		"### `clean_output`",
		"### `grounded`",
		"### `internally_consistent`",
		"### `no_fabrication`",
		"**Evaluation steps.**",
		"**Scoring bands.**",
		"- **7–10** -",
		"- **4–6** -",
		"- **0–3** -",
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

// TestRubricDocSpecsAndFixes: rubricDocSpecs/rubricDocFixes read the doc
// directly - no parser between the YAML and the envelope (#941).
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
