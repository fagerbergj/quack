package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveSkillDirs composes Resolve+SkillDirs, the way boot does - tests want
// the resolved dirs without asserting on the error return.
func resolveSkillDirs(roots []string) []string {
	plugins, _ := Resolve(roots)
	return SkillDirs(plugins)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pluginRoot lays down a root with the given plugin.json / .codex-plugin
// manifest contents (either may be "" to omit it) and the given skill names
// under the given skills subdirectory ("skills" or a custom codex target).
func pluginRoot(t *testing.T, rootJSON, codexJSON, skillsSubdir string, skills ...string) string {
	t.Helper()
	root := t.TempDir()
	if rootJSON != "" {
		writeFile(t, filepath.Join(root, "plugin.json"), rootJSON)
	}
	if codexJSON != "" {
		writeFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), codexJSON)
	}
	for _, name := range skills {
		writeFile(t, filepath.Join(root, skillsSubdir, name, "SKILL.md"), "---\nname: "+name+"\n---\nbody")
	}
	return root
}

// A root plugin.json present -> skills load from <root>/skills.
func TestResolveSkillDirs_RootManifest(t *testing.T) {
	root := pluginRoot(t, `{"$schema":"x","name":"acme"}`, "", "skills", "a", "b")
	dirs := resolveSkillDirs([]string{root})
	want := filepath.Join(root, "skills")
	if len(dirs) != 1 || dirs[0] != want {
		t.Fatalf("dirs = %v, want [%s]", dirs, want)
	}
	entries, err := os.ReadDir(dirs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("skills dir has %d entries, want 2", len(entries))
	}
}

// No root manifest, .codex-plugin/plugin.json present with
// an explicit "skills" field -> skills load from that path.
func TestResolveSkillDirs_CodexManifest(t *testing.T) {
	root := pluginRoot(t, "", `{"name":"ponytail","skills":"./skills/","interface":{"displayName":"x","weird":123}}`, "skills", "ponytail")
	dirs := resolveSkillDirs([]string{root})
	want := filepath.Join(root, "skills")
	if len(dirs) != 1 || dirs[0] != want {
		t.Fatalf("dirs = %v, want [%s]", dirs, want)
	}
}

// Both manifests present -> root plugin.json wins, the Codex manifest
// is never consulted (proven by pointing it at a skills dir that doesn't exist).
func TestResolveSkillDirs_RootWinsOverCodex(t *testing.T) {
	root := pluginRoot(t, `{"$schema":"x","name":"acme"}`, `{"name":"acme","skills":"./nonexistent/"}`, "skills", "a")
	dirs := resolveSkillDirs([]string{root})
	want := filepath.Join(root, "skills")
	if len(dirs) != 1 || dirs[0] != want {
		t.Fatalf("dirs = %v, want [%s] (root manifest should win)", dirs, want)
	}
}

// A configured root that does not exist, or has neither manifest ->
// skipped with a warning naming the root; other configured plugins still load.
func TestResolveSkillDirs_NeitherManifest_SkippedNotOthers(t *testing.T) {
	bare := t.TempDir() // exists, no manifest of either kind
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	good := pluginRoot(t, `{"$schema":"x","name":"good"}`, "", "skills", "s")

	dirs := resolveSkillDirs([]string{bare, missing, good})
	want := filepath.Join(good, "skills")
	if len(dirs) != 1 || dirs[0] != want {
		t.Fatalf("dirs = %v, want only the good root [%s]", dirs, want)
	}
}

// A Codex skills value escaping the plugin root is refused,
// the plugin skipped, not the path followed.
func TestResolveSkillDirs_CodexSkillsEscapesRoot_Refused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), `{"name":"evil","skills":"../../etc"}`)
	// Something outside root that a naive join could resolve to, proving it's
	// never touched.
	outside := filepath.Join(filepath.Dir(root), "etc")
	writeFile(t, filepath.Join(outside, "SKILL.md"), "should never be read")
	defer os.RemoveAll(outside)

	dirs := resolveSkillDirs([]string{root})
	if len(dirs) != 0 {
		t.Fatalf("dirs = %v, want empty (escape must be refused)", dirs)
	}
}

// A malformed plugin.json is skipped with a warning, other configured
// plugins still load - and does NOT fall through to a .codex-plugin manifest
// that happens to also be present (an existing but broken root manifest is terminal, not "absent").
func TestResolveSkillDirs_MalformedRootManifest_SkippedNoFallthrough(t *testing.T) {
	root := pluginRoot(t, `{not valid json`, `{"name":"x","skills":"./skills/"}`, "skills", "s")
	good := pluginRoot(t, `{"$schema":"x","name":"good"}`, "", "skills", "s")

	dirs := resolveSkillDirs([]string{root, good})
	want := filepath.Join(good, "skills")
	if len(dirs) != 1 || dirs[0] != want {
		t.Fatalf("dirs = %v, want only the good root [%s] (malformed root must not fall through to codex)", dirs, want)
	}
}

// Zero plugins configured -> empty result, no error.
func TestResolveSkillDirs_NoRoots(t *testing.T) {
	if dirs := resolveSkillDirs(nil); len(dirs) != 0 {
		t.Fatalf("dirs = %v, want empty", dirs)
	}
}

// Order is preserved and never deduped/reordered - precedence between two
// plugins defining the same skill name is left to callers/consumers.
func TestResolveSkillDirs_PreservesOrder(t *testing.T) {
	a := pluginRoot(t, `{"$schema":"x","name":"a"}`, "", "skills", "shared")
	b := pluginRoot(t, `{"$schema":"x","name":"b"}`, "", "skills", "shared")
	dirs := resolveSkillDirs([]string{a, b})
	if len(dirs) != 2 || dirs[0] != filepath.Join(a, "skills") || dirs[1] != filepath.Join(b, "skills") {
		t.Fatalf("dirs = %v, want [%s %s] in that order", dirs, filepath.Join(a, "skills"), filepath.Join(b, "skills"))
	}
}
