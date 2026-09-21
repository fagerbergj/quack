package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every Sleeper agent ships the same parts: a rubric, and a plugin skill
// that exists on disk. #1521 shipped four agents without either.
func TestSleeperPluginAgentsShipRubricAndSkill(t *testing.T) {
	agents, err := os.ReadDir(filepath.Join(sleeperPluginRoot, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		dir := filepath.Join(sleeperPluginRoot, "agents", a.Name())
		if _, err := os.Stat(filepath.Join(dir, "rubric.yaml")); err != nil {
			t.Errorf("%s: no rubric.yaml beside a judged agent", a.Name())
		}
		raw, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Skills []string `yaml:"skills"`
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("%s: agent.yaml: %v", a.Name(), err)
		}
		if len(cfg.Skills) == 0 {
			t.Errorf("%s: agent.yaml lists no skill", a.Name())
		}
		for _, s := range cfg.Skills {
			name, ok := strings.CutPrefix(s, "sleeper:")
			if !ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(sleeperPluginRoot, "skills", name, "SKILL.md")); err != nil {
				t.Errorf("%s: skill %q has no SKILL.md in the plugin", a.Name(), s)
			}
		}
	}
}
