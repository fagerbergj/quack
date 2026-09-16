package vetting

import (
	"context"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// TestFromConfigResolvesArtifacts: the gate's rubric and constitution come from
// the rubric/global and rubric/constitution artifacts when no inline override
// or path is set, which is what a run resolves at its start (#1420).
func TestFromConfigResolvesArtifacts(t *testing.T) {
	ctx := context.Background()
	c, err := FromConfig(ctx, nil, config.GatesConfig{
		ConstitutionPath: defaultConstitutionPath,
		Judge:            config.JudgeConfig{Model: "m", MaxRounds: 2, Threshold: 0.7},
	})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.Constitution == "" {
		t.Error("constitution empty; rubric/constitution did not resolve")
	}
	if c.Rubric == "" {
		t.Error("rubric empty; rubric/global did not resolve")
	}
	if c.JudgeRounds != 2 || c.Threshold != 0.7 {
		t.Errorf("config knobs not carried through: %+v", c)
	}
}

// TestFromConfigInlineOverridesWin: inline prose short-circuits resolution, so
// a planner-authored rubric is never overwritten by the shipped one.
func TestFromConfigInlineOverridesWin(t *testing.T) {
	c, err := FromConfig(context.Background(), nil, config.GatesConfig{
		Constitution: "  be kind  ",
		Rubric:       "  score it  ",
		Judge:        config.JudgeConfig{MaxRounds: 1},
	})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.Constitution != "be kind" || c.Rubric != "score it" {
		t.Errorf("inline overrides not used: %+v", c)
	}
	if c.RubricSpecs != nil {
		t.Error("a prose override has no structured specs")
	}
}

// TestFromConfigNoJudgeSkipsRubric: a deterministic-only gate needs no rubric.
func TestFromConfigNoJudgeSkipsRubric(t *testing.T) {
	c, err := FromConfig(context.Background(), nil, config.GatesConfig{})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.Rubric != "" || c.Constitution != "" {
		t.Errorf("resolved artifacts for a judge-less gate: %+v", c)
	}
}

// TestLoadBundleRubricResolvesAndAbsent: a shipped bundle's rubric.yaml comes
// back rendered; a bundle without one is absence, not an error.
func TestLoadBundleRubricResolvesAndAbsent(t *testing.T) {
	ctx := context.Background()
	got, err := LoadBundleRubric(ctx, nil, "agents/code-reviewer")
	if err != nil || !strings.Contains(got, "#") {
		t.Errorf("code-reviewer rubric = %q (%v)", got, err)
	}
	got, err = LoadBundleRubric(ctx, nil, "agents/advisor")
	if err != nil || got != "" {
		t.Errorf("advisor has no rubric.yaml; got %q (%v)", got, err)
	}
}

// TestJudgeBehaviourBadTemplate: a system/judge body that is not a valid
// template fails the round loudly instead of silently shipping an empty prompt.
func TestJudgeBehaviourBadTemplate(t *testing.T) {
	if _, _, err := judgeBehaviour(context.Background(), nil, true, true); err != nil {
		t.Fatalf("shipped system/judge must parse: %v", err)
	}
	// Every clause block must be present; a missing one is an error, not "".
	b, _, err := judgeBehaviour(context.Background(), nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, "You have no tools") || strings.Contains(b, "skill tools") {
		t.Errorf("no-tools behaviour selected the wrong blocks: %q", b)
	}
}
