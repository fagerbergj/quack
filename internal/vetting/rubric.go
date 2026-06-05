package vetting

import (
	"fmt"
	"os"
	"strings"

	"github.com/fagerbergj/quack/internal/config"
)

// FromConfig resolves the adversarial config into a gate Config, loading the
// rubric from its inline value or file. Validation of the config itself (provider
// existence, rounds/threshold ranges, exactly one rubric source) happens in
// config.validate; this just materializes the rubric text.
func FromConfig(c config.AdversarialConfig) (Config, error) {
	rubric, err := loadRubric(c)
	if err != nil {
		return Config{}, err
	}
	return Config{
		MaxRounds:  c.MaxRounds,
		Threshold:  c.Threshold,
		SelfRefine: c.SelfRefine,
		Rubric:     rubric,
	}, nil
}

func loadRubric(c config.AdversarialConfig) (string, error) {
	if r := strings.TrimSpace(c.Rubric); r != "" {
		return r, nil
	}
	raw, err := os.ReadFile(c.RubricPath)
	if err != nil {
		return "", fmt.Errorf("vetting: read rubric %q: %w", c.RubricPath, err)
	}
	r := strings.TrimSpace(string(raw))
	if r == "" {
		return "", fmt.Errorf("vetting: rubric %q is empty", c.RubricPath)
	}
	return r, nil
}
