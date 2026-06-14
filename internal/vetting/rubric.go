package vetting

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
		SelfCritiqueRounds:  c.SelfCritique.MaxRounds,
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
// directory to override the global constitution for that agent.
func LoadBundleRubric(bundleDir string) (string, error) {
	candidate := filepath.Join(bundleDir, "rubric.md")
	raw, err := os.ReadFile(candidate)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vetting: read bundle rubric %q: %w", candidate, err)
	}
	r := strings.TrimSpace(string(raw))
	if r == "" {
		return "", nil // treat empty as absent
	}
	return r, nil
}
