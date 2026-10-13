package extensions

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// resourceDirs are the only roots ADK's load_skill_resource accepts, relative to the skill directory.
var resourceDirs = []string{"references/", "assets/", "scripts/"}

var backticked = regexp.MustCompile("`([^`\\s]+)`")

// TestPluginSkillResourcesResolve: a plugin SKILL.md names each bundled resource by the path
// load_skill_resource accepts, and every such path exists; a bare file name fails the worker's load.
func TestPluginSkillResourcesResolve(t *testing.T) {
	skills, err := filepath.Glob("*/plugin/skills/*/SKILL.md")
	if err != nil || len(skills) == 0 {
		t.Fatalf("no plugin skills found (err %v)", err)
	}
	for _, path := range skills {
		dir := filepath.Dir(path)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		bundled := bundledResources(t, dir)
		for _, m := range backticked.FindAllStringSubmatch(string(body), -1) {
			ref := m[1]
			if hasResourceRoot(ref) {
				if _, err := os.Stat(filepath.Join(dir, ref)); err != nil && !strings.Contains(ref, "*") {
					t.Errorf("%s: `%s` does not exist", path, ref)
				}
			} else if root, ok := bundled[ref]; ok {
				t.Errorf("%s: `%s` must be named `%s%s`, the path load_skill_resource accepts", path, ref, root, ref)
			}
		}
	}
}

// bundledResources maps each top-level file name under dir's resource roots to its root.
func bundledResources(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range resourceDirs {
		entries, err := os.ReadDir(filepath.Join(dir, root))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, e := range entries {
			out[e.Name()] = root
		}
	}
	return out
}

func hasResourceRoot(ref string) bool {
	for _, root := range resourceDirs {
		if strings.HasPrefix(ref, root) {
			return true
		}
	}
	return false
}
