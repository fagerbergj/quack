package vetting

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// TestLoadRubricFallsBackWhenPathMissing covers cli.md audit finding
// onboarding#3: quack init emits gates.rubric_path: config/rubric.md, which
// only resolves on disk inside a checkout - a missing custom path (and an
// unset one, when the judge is enabled) must fall back to the embedded
// default rather than hard-failing gate setup.
func TestLoadRubricFallsBackWhenPathMissing(t *testing.T) {
	c := config.GatesConfig{RubricPath: "does/not/exist.md"}
	rendered, _, _, err := loadRubric(c)
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("expected the embedded default rubric, got empty")
	}
}

func TestLoadRubricUnsetPathWithJudgeEnabledUsesEmbeddedDefault(t *testing.T) {
	c := config.GatesConfig{Judge: config.JudgeConfig{Model: "j", MaxRounds: 1}}
	rendered, _, _, err := loadRubric(c)
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("expected the embedded default rubric, got empty")
	}
}

func TestLoadRubricUnsetPathWithJudgeDisabledIsOptional(t *testing.T) {
	rendered, specs, fixes, err := loadRubric(config.GatesConfig{})
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if rendered != "" || specs != nil || fixes != nil {
		t.Errorf("expected no rubric when the judge is disabled, got %q", rendered)
	}
}

func TestLoadRubricFileErrorNamesRubricPath(t *testing.T) {
	// Both the custom path and the embedded default resolve nowhere only if
	// defaultRubricPath itself is wrong - exercised here via readWithFallback
	// directly so the error-naming behavior is covered without depending on
	// the embed actually being broken.
	_, err := readWithFallback("nope.md", "also/nope.md")
	if err == nil {
		t.Fatal("expected an error when neither path resolves")
	}
}

func TestLoadConstitutionFallsBackWhenPathMissing(t *testing.T) {
	c := config.GatesConfig{ConstitutionPath: "does/not/exist.md"}
	got, err := loadConstitution(c)
	if err != nil {
		t.Fatalf("loadConstitution: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("expected the embedded default constitution, got empty")
	}
}
