package vetting

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
)

// hardErrSource always fails with artifactsrc.ErrHard, the one class of
// resolver failure that propagates instead of falling back to the shipped file.
type hardErrSource struct{}

func (hardErrSource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{}, false, fmt.Errorf("pinned version gone: %w", artifactsrc.ErrHard)
}
func (hardErrSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// bodySource always answers ok with a fixed body, for exercising a rubric.yaml's
// parse/render branches without a real store.
type bodySource struct{ body string }

func (b bodySource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{Body: b.body}, true, nil
}
func (bodySource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

func TestLoadRubricFallsBackWhenPathMissing(t *testing.T) {
	c := config.GatesConfig{RubricPath: "does/not/exist.md"}
	rendered, _, _, _, err := loadRubric(context.Background(), nil, c)
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("expected the embedded default rubric, got empty")
	}
}

func TestLoadRubricUnsetPathWithJudgeEnabledUsesEmbeddedDefault(t *testing.T) {
	c := config.GatesConfig{Judge: config.JudgeConfig{Model: "j", MaxRounds: 1}}
	rendered, _, _, _, err := loadRubric(context.Background(), nil, c)
	if err != nil {
		t.Fatalf("loadRubric: %v", err)
	}
	if strings.TrimSpace(rendered) == "" {
		t.Fatal("expected the embedded default rubric, got empty")
	}
}

func TestLoadRubricUnsetPathWithJudgeDisabledIsOptional(t *testing.T) {
	rendered, specs, fixes, _, err := loadRubric(context.Background(), nil, config.GatesConfig{})
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
	_, _, err := readWithFallback(context.Background(), nil, "nope.md", "also/nope.md")
	if err == nil {
		t.Fatal("expected an error when neither path resolves")
	}
}

func TestLoadConstitutionFallsBackWhenPathMissing(t *testing.T) {
	c := config.GatesConfig{ConstitutionPath: "does/not/exist.md"}
	got, _, err := loadConstitution(context.Background(), nil, c)
	if err != nil {
		t.Fatalf("loadConstitution: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("expected the embedded default constitution, got empty")
	}
}

// TestReadWithFallbackReadsCustomPathFromDisk: a custom path that DOES resolve
// on disk never goes through artifactsrc, but still gets a real, content-hashed
// Artifact (FileArtifact) - never an unattributed one.
func TestReadWithFallbackReadsCustomPathFromDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.md")
	if err := os.WriteFile(p, []byte("custom content"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, art, err := readWithFallback(context.Background(), nil, p, "unused-default")
	if err != nil {
		t.Fatalf("readWithFallback: %v", err)
	}
	if string(raw) != "custom content" {
		t.Errorf("raw = %q, want custom content", raw)
	}
	if art.Name != p || art.Source != artifactsrc.StaticSource || art.VersionID == "" {
		t.Errorf("art = %+v, want name %q, source static, a non-empty version id", art, p)
	}
}

// TestLoadConstitutionPropagatesHardResolverError: a resolver failure that
// isn't a missing name must surface, not silently fall back.
func TestLoadConstitutionPropagatesHardResolverError(t *testing.T) {
	res := artifactsrc.New("langfuse", hardErrSource{}, time.Minute)
	c := config.GatesConfig{ConstitutionPath: defaultConstitutionPath}
	if _, _, err := loadConstitution(context.Background(), res, c); err == nil {
		t.Fatal("want an error")
	}
}

// TestLoadRubricFilePropagatesHardResolverError mirrors the constitution case for the rubric path.
func TestLoadRubricFilePropagatesHardResolverError(t *testing.T) {
	res := artifactsrc.New("langfuse", hardErrSource{}, time.Minute)
	if _, _, _, _, err := loadRubricFile(context.Background(), res, defaultRubricPath); err == nil {
		t.Fatal("want an error")
	}
}

// TestLoadRubricFileCustomYAMLPath covers the .yaml-suffixed custom-path branch:
// a valid custom rubric.yaml parses and renders; an invalid one surfaces the parse error.
func TestLoadRubricFileCustomYAMLPath(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(valid, []byte("criteria: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, art, err := loadRubricFile(context.Background(), nil, valid); err != nil || art.Name != valid || art.VersionID == "" {
		t.Errorf("loadRubricFile(valid custom yaml) = art %+v err %v, want a content-hashed FileArtifact named %q, no error", art, err, valid)
	}

	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := loadRubricFile(context.Background(), nil, broken); err == nil {
		t.Fatal("want a parse error for invalid YAML")
	}
}

// TestLoadBundleRubricSpecs_ErrorBranches: a generic (non-fs.ErrNotExist) resolver
// failure surfaces; a store-provided rubric.yaml that fails to parse surfaces too;
// one that parses but renders empty (no criteria) is treated as absent.
func TestLoadBundleRubricSpecs_ErrorBranches(t *testing.T) {
	ctx := context.Background()
	hardRes := artifactsrc.New("langfuse", hardErrSource{}, time.Minute)
	if _, _, _, _, err := LoadBundleRubricSpecs(ctx, hardRes, "agents/web-researcher"); err == nil {
		t.Fatal("want an error for a hard resolver failure")
	}

	brokenRes := artifactsrc.New("langfuse", bodySource{body: "not: [valid"}, time.Minute)
	if _, _, _, _, err := LoadBundleRubricSpecs(ctx, brokenRes, "agents/web-researcher"); err == nil {
		t.Fatal("want a parse error for invalid stored YAML")
	}

	emptyRes := artifactsrc.New("langfuse", bodySource{body: "criteria: {}\n"}, time.Minute)
	rendered, specs, fixes, art, err := LoadBundleRubricSpecs(ctx, emptyRes, "agents/web-researcher")
	if err != nil || rendered != "" || specs != nil || fixes != nil || art.Name != "" {
		t.Errorf("LoadBundleRubricSpecs(no criteria) = (%q, %v, %v, %+v, %v), want all zero", rendered, specs, fixes, art, err)
	}
}
