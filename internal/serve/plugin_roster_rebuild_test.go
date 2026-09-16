package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestRebuildSkillsPicksUpNewlyRegisteredPlugin is #1430 P2's roster-rebuild
// requirement: a plugin added to the registry AFTER boot (REST's job) must
// become visible to native agents' already-built skill roster on the very
// next call - no restart, no re-building the SkillToolset itself - once the
// rebuild hook initSkills returns is invoked.
func TestRebuildSkillsPicksUpNewlyRegisteredPlugin(t *testing.T) {
	registryRoot := t.TempDir()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: registryRoot}}}

	skills, err := b.initSkills(context.Background(), jail)
	if err != nil {
		t.Fatalf("initSkills: %v", err)
	}
	builtinSkillSrc, rebuildSkills := skills.builtinSkillSrc, skills.rebuildSkills
	// A REST-style add: write a local plugin root (plugin.json + skills/) and
	// Put it into the SAME registry initSkills resolved against - no git needed
	// for a local entry. The registry row name is the entry's own base name
	// (resolveRegistryPlugins stamps THAT, never plugin.json's, #1427 S3).
	pluginRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte(`{"name":"extra"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeVendorSkill(t, filepath.Join(pluginRoot, "skills"), "widget-maker", "Makes widgets.")
	entry, err := pluginreg.ParseEntry(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	skillName := entry.Name() + ":widget-maker"

	if _, err := builtinSkillSrc.LoadFrontmatter(context.Background(), skillName); err == nil {
		t.Fatalf("%s should not exist before the plugin is registered", skillName)
	}

	reg := pluginreg.NewFSRegistry(registryRoot)
	if err := reg.Put(context.Background(), pluginreg.FromEntry(entry)); err != nil {
		t.Fatal(err)
	}

	if err := rebuildSkills(); err != nil {
		t.Fatalf("rebuildSkills: %v", err)
	}

	fm, err := builtinSkillSrc.LoadFrontmatter(context.Background(), skillName)
	if err != nil {
		t.Fatalf("LoadFrontmatter after rebuild: %v", err)
	}
	if fm.Name != skillName {
		t.Fatalf("frontmatter name = %q, want %s", fm.Name, skillName)
	}
}
