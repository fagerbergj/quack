package serve

import (
	"bytes"
	"os"
	"os/exec"
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

// A skill's output schema is the page's contract, so each copy must be
// byte-identical to a schema the pinned extension module ships.
func TestSleeperSkillSchemasMatchPinnedExtension(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/fagerbergj/quack-extensions/sleeper").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	shipped, err := filepath.Glob(filepath.Join(strings.TrimSpace(string(out)), "ui", "schemas", "*.json"))
	if err != nil || len(shipped) == 0 {
		t.Fatalf("no schemas in the pinned module: %v", err)
	}
	copies, err := filepath.Glob(filepath.Join(sleeperPluginRoot, "skills", "*", "references", "output-schema*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) < 9 {
		t.Fatalf("found %d schema references, want one per job kind the agents write (9)", len(copies))
	}
	for _, c := range copies {
		got, err := os.ReadFile(c)
		if err != nil {
			t.Fatal(err)
		}
		if !matchesAny(got, shipped) {
			t.Errorf("%s matches no schema in the pinned extension; re-copy it from ui/schemas", c)
		}
	}
}

func matchesAny(got []byte, files []string) bool {
	for _, f := range files {
		if want, err := os.ReadFile(f); err == nil && bytes.Equal(got, want) {
			return true
		}
	}
	return false
}
