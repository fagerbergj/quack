package vetting

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/config"
)

// defaultArtifact names the two shipped gate fallbacks as artifacts, so an
// override path that resolves nowhere lands on the resolver, not a raw file read.
var defaultArtifact = map[string]string{
	defaultRubricPath:       "rubric/global",
	defaultConstitutionPath: "rubric/constitution",
}

// readWithFallback: a path that resolves nowhere falls back to defaultPath's
// artifact instead of hard-failing.
func readWithFallback(ctx context.Context, res *artifactsrc.Resolver, path, defaultPath string) ([]byte, error) {
	if path != defaultPath {
		if raw, err := bundledir.ReadFile(path); err == nil {
			return raw, nil
		}
	}
	art, err := res.Resolve(ctx, defaultArtifact[defaultPath])
	if err != nil {
		return nil, err
	}
	return []byte(art.Body), nil
}

// FromConfig resolves the gates config into a gate Config, loading the
// constitution (optional global principles) and rubric (scoring guide) from
// their inline values or artifacts. Called at the start of every run rather than
// once at boot, so an edited rubric reaches the next node without a restart.
// Validation of the config itself happens in config.validate; this just materialises the text.
func FromConfig(ctx context.Context, res *artifactsrc.Resolver, c config.GatesConfig) (Config, error) {
	constitution, err := loadConstitution(ctx, res, c)
	if err != nil {
		return Config{}, err
	}
	rubric, specs, fixes, err := loadRubric(ctx, res, c)
	if err != nil {
		return Config{}, err
	}
	return Config{
		DeterministicRounds:  c.DeterministicChecks.MaxRounds,
		JudgeRounds:          c.Judge.MaxRounds,
		Threshold:            c.Judge.Threshold,
		JudgeMaxIterations:   c.Judge.MaxIterations,
		JudgeContextWindow:   c.Judge.ContextWindow,
		Constitution:         constitution,
		Rubric:               rubric,
		RubricSpecs:          specs,
		RubricFixes:          fixes,
		JudgeMaxOutputTokens: c.Judge.MaxOutputTokens,
		JudgeThinkingLevel:   c.Judge.ThinkingLevel,
	}, nil
}

// defaultRubricPath/defaultConstitutionPath match what quack init emits and
// what embed.go embeds - the fallback target.
const (
	defaultRubricPath       = "config/rubric.md"
	defaultConstitutionPath = "config/constitution.md"
)

func loadConstitution(ctx context.Context, res *artifactsrc.Resolver, c config.GatesConfig) (string, error) {
	if r := strings.TrimSpace(c.Constitution); r != "" {
		return r, nil
	}
	if c.ConstitutionPath == "" {
		return "", nil // constitution is optional
	}
	raw, err := readWithFallback(ctx, res, c.ConstitutionPath, defaultConstitutionPath)
	if err != nil {
		return "", fmt.Errorf("vetting: read constitution %q (set by gates.constitution_path): %w", c.ConstitutionPath, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// loadRubric returns the rendered rubric markdown for the judge prompt, and
// (when the source was a rubric.yaml, not a raw override) the per-criterion
// specs the envelope needs. A planner/inline-config rubric override is unstructured prose - specs is nil in that case (#941).
func loadRubric(ctx context.Context, res *artifactsrc.Resolver, c config.GatesConfig) (string, map[string]criterionSpec, map[string]string, error) {
	if r := strings.TrimSpace(c.Rubric); r != "" {
		return r, nil, nil, nil // raw inline override - unstructured prose, no specs
	}
	path := c.RubricPath
	if path == "" {
		if !c.JudgeEnabled() {
			return "", nil, nil, nil // rubric is optional for a deterministic-only gate
		}
		path = defaultRubricPath
	}
	return loadRubricFile(ctx, res, path)
}

// loadRubricFile loads a rubric from disk: a .yaml path is the structured
// format (rubricyaml.go); anything else is a raw prose override with no
// structured specs. A path that doesn't resolve falls back to the embedded default.
func loadRubricFile(ctx context.Context, res *artifactsrc.Resolver, path string) (string, map[string]criterionSpec, map[string]string, error) {
	raw, err := readWithFallback(ctx, res, path, defaultRubricPath)
	if err != nil {
		return "", nil, nil, fmt.Errorf("vetting: read rubric %q (set by gates.rubric_path): %w", path, err)
	}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		doc, err := loadRubricYAML(raw, path)
		if err != nil {
			return "", nil, nil, err
		}
		return renderRubricMarkdown(doc), rubricDocSpecs(doc), rubricDocFixes(doc), nil
	}
	r := strings.TrimSpace(string(raw))
	if r == "" {
		return "", nil, nil, fmt.Errorf("vetting: rubric %q is empty", path)
	}
	return r, nil, nil, nil
}

// LoadBundleRubric looks for a rubric.yaml file in the agent bundle directory
// and returns its rendered markdown (for callers - guidance prose, judge prompt text - that only want text) plus its structured specs (nil if the
// bundle has no rubric). "" rendered text ⇒ no per-agent rubric (not an error; caller falls back to the global constitution). Resolved from disk in cwd first, then the embedded copy (so an installed binary works).
func LoadBundleRubric(ctx context.Context, res *artifactsrc.Resolver, bundleDir string) (string, error) {
	rendered, _, _, err := LoadBundleRubricSpecs(ctx, res, bundleDir)
	return rendered, err
}

// LoadBundleRubricSpecs is LoadBundleRubric plus the structured per-criterion
// specs and deterministic fixes the envelope needs (nil when the bundle has
// no rubric.yaml).
func LoadBundleRubricSpecs(ctx context.Context, res *artifactsrc.Resolver, bundleDir string) (string, map[string]criterionSpec, map[string]string, error) {
	raw, err := artifactsrc.ReadBundleFile(ctx, res, "rubric", bundleDir, "rubric.yaml")
	if err != nil {
		// Absent on both disk and embedded ⇒ no per-agent rubric (not an error).
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, nil, nil
		}
		return "", nil, nil, fmt.Errorf("vetting: read bundle rubric %q: %w", bundleDir, err)
	}
	doc, err := loadRubricYAML(raw, bundledir.PathJoin(bundleDir, "rubric.yaml"))
	if err != nil {
		return "", nil, nil, err
	}
	rendered := renderRubricMarkdown(doc)
	if rendered == "" {
		return "", nil, nil, nil // treat empty as absent
	}
	return rendered, rubricDocSpecs(doc), rubricDocFixes(doc), nil
}
