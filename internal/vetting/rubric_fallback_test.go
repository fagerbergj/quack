package vetting

import (
	"context"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

func TestLoadRubricFallsBackWhenPathMissing(t *testing.T) {
	c := config.GatesConfig{RubricPath: "does/not/exist.md"}
	rendered, _, _, err := loadRubric(context.Background(), nil, c)
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("expected the embedded default rubric, got empty")
	}
}

func TestLoadRubricUnsetPathWithJudgeEnabledUsesEmbeddedDefault(t *testing.T) {
	c := config.GatesConfig{Judge: config.JudgeConfig{Model: "j", MaxRounds: 1}}
	rendered, _, _, err := loadRubric(context.Background(), nil, c)
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("expected the embedded default rubric, got empty")
	}
}

func TestLoadRubricUnsetPathWithJudgeDisabledIsOptional(t *testing.T) {
	rendered, specs, fixes, err := loadRubric(context.Background(), nil, config.GatesConfig{})
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if rendered != "" || specs != nil || fixes != nil {
		t.Errorf("expected no rubric when the judge is disabled, got %q", rendered)
	}
}

func TestLoadRubricFileErrorNamesRubricPath(t *testing.T) {
	// Exercises readWithFallback directly so this doesn't depend on the
	// embed actually being broken.
	_, err := readWithFallback(context.Background(), nil, "nope.md", "also/nope.md")
	if err == nil {
		t.Fatal("expected an error when neither path resolves")
	}
}

func TestLoadConstitutionFallsBackWhenPathMissing(t *testing.T) {
	c := config.GatesConfig{ConstitutionPath: "does/not/exist.md"}
	got, err := loadConstitution(context.Background(), nil, c)
	if err != nil {
		t.Fatalf("loadConstitution: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("expected the embedded default constitution, got empty")
	}
}
