package vetting

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/config"
)

// FromConfig resolves the gates config into a gate Config, loading the
// constitution (optional global principles) and rubric (scoring guide) from
// their inline values or files. Validation of the config itself happens in
// config.validate; this just materialises the text.
func FromConfig(c config.GatesConfig) (Config, error) {
	constitution, err := loadConstitution(c)
	if err != nil {
		return Config{}, err
	}
	rubric, specs, fixes, err := loadRubric(c)
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
		SkepticRounds:        c.Judge.Skeptics,
		JudgeMaxOutputTokens: c.Judge.MaxOutputTokens,
	}, nil
}

func loadConstitution(c config.GatesConfig) (string, error) {
	if r := strings.TrimSpace(c.Constitution); r != "" {
		return r, nil
	}
	if c.ConstitutionPath == "" {
		return "", nil // constitution is optional
	}
	raw, err := os.ReadFile(c.ConstitutionPath)
	if err != nil {
		return "", fmt.Errorf("vetting: read constitution %q: %w", c.ConstitutionPath, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// loadRubric returns the rendered rubric markdown for the judge prompt, and
// (when the source was a rubric.yaml, not a raw override) the per-criterion
// specs the envelope needs. A planner/inline-config rubric override is
// unstructured prose - specs is nil in that case (#941).
func loadRubric(c config.GatesConfig) (string, map[string]criterionSpec, map[string]string, error) {
	if r := strings.TrimSpace(c.Rubric); r != "" {
		return r, nil, nil, nil // raw inline override - unstructured prose, no specs
	}
	if c.RubricPath == "" {
		return "", nil, nil, nil // rubric is optional for a deterministic-only gate
	}
	return loadRubricFile(c.RubricPath)
}

// loadRubricFile loads a rubric from disk: a .yaml path is the structured
// format (rubricyaml.go); anything else is a raw prose override with no
// structured specs.
func loadRubricFile(path string) (string, map[string]criterionSpec, map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, nil, fmt.Errorf("vetting: read rubric %q: %w", path, err)
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
// and returns its rendered markdown (for callers - guidance prose, judge
// prompt text - that only want text) plus its structured specs (nil if the
// bundle has no rubric). "" rendered text ⇒ no per-agent rubric (not an
// error; caller falls back to the global constitution). Resolved from disk in
// cwd first, then the embedded copy (so an installed binary works).
func LoadBundleRubric(bundleDir string) (string, error) {
	rendered, _, _, err := LoadBundleRubricSpecs(bundleDir)
	return rendered, err
}

// LoadBundleRubricSpecs is LoadBundleRubric plus the structured per-criterion
// specs and deterministic fixes the envelope needs (nil when the bundle has
// no rubric.yaml).
func LoadBundleRubricSpecs(bundleDir string) (string, map[string]criterionSpec, map[string]string, error) {
	raw, err := bundledir.ReadFile(bundledir.PathJoin(bundleDir, "rubric.yaml"))
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
