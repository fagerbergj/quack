package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Extension plugin agents name builtins by bare name, and a plugin/<name> tag can ship apart from the image:
// renaming or removing a builtin they use must fail here, not when the tagged plugin seeds.
func TestExtensionPluginAgentsNameExistingBuiltins(t *testing.T) {
	files, err := filepath.Glob("../../extensions/*/plugin/agents/*/agent.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no extension plugin agents found: %v", err)
	}
	for _, f := range files {
		module := strings.Split(filepath.ToSlash(f), "/")[3]
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var spec struct {
			Tools []string `yaml:"tools"`
		}
		if err := yaml.Unmarshal(b, &spec); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, name := range spec.Tools {
			if _, ok := registry[name]; !ok && !strings.HasPrefix(name, module+"_") {
				t.Errorf("%s: tool %q is neither a builtin nor a %s_ extension tool", f, name, module)
			}
		}
	}
}
