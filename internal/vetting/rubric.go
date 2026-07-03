package vetting

import (
	"fmt"
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
	rubric, err := loadRubric(c)
	if err != nil {
		return Config{}, err
	}
	return Config{
		DeterministicRounds: c.DeterministicChecks.MaxRounds,
		JudgeRounds:         c.Judge.MaxRounds,
		Threshold:           c.Judge.Threshold,
		JudgeMaxIterations:  c.Judge.MaxIterations,
		Constitution:        constitution,
		Rubric:              rubric,
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

func loadRubric(c config.GatesConfig) (string, error) {
	if r := strings.TrimSpace(c.Rubric); r != "" {
		return r, nil
	}
	if c.RubricPath == "" {
		return "", nil // rubric is optional for a deterministic-only gate
	}
	return loadRubricFile(c.RubricPath)
}

func loadRubricFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("vetting: read rubric %q: %w", path, err)
	}
	r := strings.TrimSpace(string(raw))
	if r == "" {
		return "", fmt.Errorf("vetting: rubric %q is empty", path)
	}
	return r, nil
}

// LoadBundleRubric looks for a rubric.md file in the agent bundle directory.
// Returns the rubric text if found, or "" if absent (caller falls back to the
// global constitution). The convention is: global constitution = foundational
// principles shared across all agents; per-agent rubric = domain-specific
// scoring criteria for this agent's work. Drop a rubric.md into a bundle
// directory to override the global constitution for that agent. Resolved from
// disk in cwd first, then the embedded copy (so an installed binary works).
func LoadBundleRubric(bundleDir string) (string, error) {
	raw, err := bundledir.ReadFile(bundledir.PathJoin(bundleDir, "rubric.md"))
	if err != nil {
		// Absent on both disk and embedded ⇒ no per-agent rubric (not an error).
		if isNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("vetting: read bundle rubric %q: %w", bundleDir, err)
	}
	r := strings.TrimSpace(string(raw))
	if r == "" {
		return "", nil // treat empty as absent
	}
	return r, nil
}

// isNotExist reports whether err is a not-exist error from either os or embed.
func isNotExist(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "does not exist"))
}
