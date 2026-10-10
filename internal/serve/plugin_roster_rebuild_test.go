package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/workspace"
)

// rebuildSkills reloads a skills-only reloader and keys its admission refusals by row name.
func rebuildSkills(s skillsInit) (map[string]string, error) {
	rep, err := s.reload.reload(context.Background())
	refused := map[string]string{}
	for _, f := range rep.Failures {
		if f.Stage == schema.Admission && f.Plugin != nil {
			refused[*f.Plugin] = f.Error
		}
	}
	return refused, err
}

// TestRebuildSkillsPicksUpNewlyRegisteredPlugin: a plugin added to the registry after boot shows up in
// native agents' skill roster on the next call once the rebuild hook runs, with no restart.
func TestRebuildSkillsPicksUpNewlyRegisteredPlugin(t *testing.T) {
	registryRoot := t.TempDir()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: registryRoot}}}

	skills, err := b.initSkills(context.Background(), jail, nil, nil)
	if err != nil {
		t.Fatalf("initSkills: %v", err)
	}
	builtinSkillSrc := skills.builtinSkillSrc
	// A REST-style add of a local plugin into the same registry initSkills used (no git needed).
	// The row name is the entry's base name, never plugin.json's.
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

	if refusals, err := rebuildSkills(skills); err != nil || len(refusals) != 0 {
		t.Fatalf("rebuildSkills = (%v, %v), want no refusals and no error", refusals, err)
	}

	fm, err := builtinSkillSrc.LoadFrontmatter(context.Background(), skillName)
	if err != nil {
		t.Fatalf("LoadFrontmatter after rebuild: %v", err)
	}
	if fm.Name != skillName {
		t.Fatalf("frontmatter name = %q, want %s", fm.Name, skillName)
	}
}

// TestRebuildSkillsDropsOnlyTheRefusedRow: rebuildSkills uses boot's per-row admission, so a refused non-seed
// row is dropped (error persisted on its own row) while everything else still rebuilds and swaps.
func TestRebuildSkillsDropsOnlyTheRefusedRow(t *testing.T) {
	registryRoot := t.TempDir()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: registryRoot}}}
	skills, err := b.initSkills(context.Background(), jail, nil, nil)
	if err != nil {
		t.Fatalf("initSkills: %v", err)
	}

	before, err := skills.builtinSkillSrc.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	badRoot := t.TempDir()
	writeGhostPluginManifest(t, badRoot, "bad")
	entry, err := pluginreg.ParseEntry(badRoot)
	if err != nil {
		t.Fatal(err)
	}
	reg := pluginreg.NewFSRegistry(registryRoot)
	if err := reg.Put(context.Background(), pluginreg.FromEntry(entry)); err != nil {
		t.Fatal(err)
	}

	refusals, err := rebuildSkills(skills)
	if err != nil {
		t.Fatalf("rebuildSkills = %v, want nil (only a seed refusal is fatal)", err)
	}
	if refusals[entry.Name()] == "" {
		t.Fatalf("refusals = %v, want the bad row named", refusals)
	}

	after, err := skills.builtinSkillSrc.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("roster after dropping the one bad row: before=%d after=%d, want unchanged (the good plugins reload)", len(before), len(after))
	}

	row, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range row {
		if r.Name == entry.Name() {
			found = true
			if r.Error == "" {
				t.Errorf("bad row.Error = %q, want the refusal persisted", r.Error)
			}
		}
	}
	if !found {
		t.Fatalf("bad row %q missing from the registry after a refused (not fatal) rebuild", entry.Name())
	}
}

// TestRebuildSkillsPreExistingRefusalDoesNotBlockAnUnrelatedAdd: a row refused on an earlier rebuild
// doesn't fail a later rebuild triggered by an unrelated good add.
func TestRebuildSkillsPreExistingRefusalDoesNotBlockAnUnrelatedAdd(t *testing.T) {
	registryRoot := t.TempDir()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: registryRoot}}}
	skills, err := b.initSkills(context.Background(), jail, nil, nil)
	if err != nil {
		t.Fatalf("initSkills: %v", err)
	}
	reg := pluginreg.NewFSRegistry(registryRoot)

	badRoot := t.TempDir()
	writeGhostPluginManifest(t, badRoot, "bad")
	badEntry, err := pluginreg.ParseEntry(badRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(context.Background(), pluginreg.FromEntry(badEntry)); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildSkills(skills); err != nil {
		t.Fatalf("first rebuild (establishing the refusal) = %v, want nil", err)
	}

	goodRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(goodRoot, "plugin.json"), []byte(`{"name":"good"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeVendorSkill(t, filepath.Join(goodRoot, "skills"), "widget-maker", "Makes widgets.")
	goodEntry, err := pluginreg.ParseEntry(goodRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(context.Background(), pluginreg.FromEntry(goodEntry)); err != nil {
		t.Fatal(err)
	}

	refusals, err := rebuildSkills(skills)
	if err != nil {
		t.Fatalf("rebuild after an unrelated add = %v, want nil (the pre-existing refusal is not fatal)", err)
	}
	if refusals[badEntry.Name()] == "" {
		t.Errorf("refusals = %v, want the still-bad row named again", refusals)
	}

	skillName := goodEntry.Name() + ":widget-maker"
	if _, err := skills.builtinSkillSrc.LoadFrontmatter(context.Background(), skillName); err != nil {
		t.Fatalf("LoadFrontmatter(%s) = %v, want the unrelated good plugin to load", skillName, err)
	}
}
