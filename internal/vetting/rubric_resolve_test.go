package vetting

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
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
	// With no prompts: store configured, both artifacts' provenance
	// is the shipped file - "static", not blank.
	if c.ConstitutionArtifact.Name != "rubric/constitution" || c.ConstitutionArtifact.Source != "static" || c.ConstitutionArtifact.VersionID == "" {
		t.Errorf("ConstitutionArtifact = %+v, want {rubric/constitution static <hash>}", c.ConstitutionArtifact)
	}
	if c.RubricArtifact.Name != "rubric/global" || c.RubricArtifact.Source != "static" || c.RubricArtifact.VersionID == "" {
		t.Errorf("RubricArtifact = %+v, want {rubric/global static <hash>}", c.RubricArtifact)
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

// TestLoadBundleRubricSpecsArtifactProvenance: the per-agent rubric.yaml
// artifact is "static" and version-hashed when present, and zero when absent
// (a bundle with no rubric.yaml records no rubric/<agent> entry at all).
func TestLoadBundleRubricSpecsArtifactProvenance(t *testing.T) {
	ctx := context.Background()
	_, _, _, art, err := LoadBundleRubricSpecs(ctx, nil, "agents/code-reviewer")
	if err != nil {
		t.Fatalf("LoadBundleRubricSpecs: %v", err)
	}
	if art.Name != "rubric/code-reviewer" || art.Source != "static" || art.VersionID == "" {
		t.Errorf("code-reviewer rubric artifact = %+v, want {rubric/code-reviewer static <hash>}", art)
	}
	_, _, _, art, err = LoadBundleRubricSpecs(ctx, nil, "agents/advisor")
	if err != nil {
		t.Fatalf("LoadBundleRubricSpecs: %v", err)
	}
	if art.Name != "" {
		t.Errorf("advisor has no rubric.yaml; artifact = %+v, want zero", art)
	}
}

// TestJudgeBehaviourSelectsBlocks: the shipped system/judge renders every
// clause, and only the ones this judge's tools earn it reach the prompt.
func TestJudgeBehaviourSelectsBlocks(t *testing.T) {
	p, err := resolveJudgePrompt(context.Background(), nil)
	if err != nil {
		t.Fatalf("shipped system/judge must render: %v", err)
	}
	if p.art.Source != artifactsrc.StaticSource || p.art.VersionID == "" {
		t.Errorf("provenance = %+v, want the shipped file with a version id", p.art)
	}
	b := p.behaviour(false, false)
	if !strings.Contains(b, "You have no tools") || strings.Contains(b, "skill tools") {
		t.Errorf("no-tools behaviour selected the wrong blocks: %q", b)
	}
}

// proseSource stands in for someone pasting plain prose over system/judge in a
// prompt UI - no define blocks, so every clause would render empty.
type proseSource struct{}

func (proseSource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{Body: "just be a good judge, thanks", VersionID: "prose"}, true, nil
}
func (proseSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// TestJudgePromptSurvivesBadStoredVersion: a stored system/judge that will not
// render falls back to the shipped file. Erroring instead would fail every
// judge round, i.e. disable the trust gate deployment-wide.
func TestJudgePromptSurvivesBadStoredVersion(t *testing.T) {
	res := artifactsrc.New("langfuse", proseSource{}, time.Minute)
	p, err := resolveJudgePrompt(context.Background(), res)
	if err != nil {
		t.Fatalf("resolve = %v, want a fall back to the shipped file", err)
	}
	if p.art.Source != artifactsrc.StaticSource {
		t.Errorf("source = %q, want the shipped file", p.art.Source)
	}
	if b := p.behaviour(true, true); !strings.Contains(b, "read-only workspace tools") {
		t.Errorf("behaviour = %q, want the shipped clauses", b)
	}
}
