package serve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/workspace"
)

// writeGhostPluginManifest lays down a root plugin.json declaring a module
// this binary never links - the standard refusal checkModuleLinked names.
func writeGhostPluginManifest(t *testing.T, root, name string) {
	t.Helper()
	body := `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"` + name + `",` +
		`"extensions":{"io.github.fagerbergj.quack":{"schemaVersion":1,` +
		`"modules":[{"name":"ghost","path":"github.com/fagerbergj/quack-extensions/ghost"}]}}}`
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAdmitBootPluginsDropsRESTAddedRefusalKeepsBootAlive is the adversarial-
// review regression: a plugin refused by rebuildSkills (422, row kept with
// its error) must not brick the NEXT boot. A row added over REST (not in
// plugins.seed) that fails checkPlugin is dropped with a warning and its
// error persisted; boot proceeds and every other plugin still loads.
func TestAdmitBootPluginsDropsRESTAddedRefusalKeepsBootAlive(t *testing.T) {
	registryRoot := t.TempDir()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	goodRoot := t.TempDir()
	writePluginManifest(t, goodRoot, "good")
	writeVendorSkill(t, filepath.Join(goodRoot, "skills"), "widget-maker", "Makes widgets.")

	badRoot := t.TempDir()
	writeGhostPluginManifest(t, badRoot, "bad")

	reg := pluginreg.NewFSRegistry(registryRoot)
	for _, root := range []string{goodRoot, badRoot} {
		entry, err := pluginreg.ParseEntry(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Put(context.Background(), pluginreg.FromEntry(entry)); err != nil {
			t.Fatal(err)
		}
	}
	badEntry, err := pluginreg.ParseEntry(badRoot)
	if err != nil {
		t.Fatal(err)
	}
	goodEntry, err := pluginreg.ParseEntry(goodRoot)
	if err != nil {
		t.Fatal(err)
	}

	// No plugins.seed: both rows are REST-added operator data.
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: registryRoot}}}
	skills, err := b.initSkills(context.Background(), jail, nil)
	if err != nil {
		t.Fatalf("initSkills must not fail boot on a REST-added refusal: %v", err)
	}

	goodSkill := goodEntry.Name() + ":widget-maker"
	if _, err := skills.builtinSkillSrc.LoadFrontmatter(context.Background(), goodSkill); err != nil {
		t.Errorf("the OTHER plugin's skill should still load: %v", err)
	}

	rows, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := findRowByName(rows, badEntry.Name())
	if !ok {
		t.Fatalf("bad row %q missing from the registry after boot", badEntry.Name())
	}
	if row.Error == "" || !strings.Contains(row.Error, "not linked") || !strings.Contains(row.Error, "ghost") {
		t.Fatalf("bad row.Error = %q, want the unlinked-module refusal stored", row.Error)
	}
}

// TestAdmitBootPluginsFailsBootForSeedListedRefusal: the SAME manifest,
// listed in plugins.seed (config), still fails boot fatally and names it -
// unchanged from before this fix.
func TestAdmitBootPluginsFailsBootForSeedListedRefusal(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	badRoot := t.TempDir()
	writeGhostPluginManifest(t, badRoot, "bad")

	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{
		Root: t.TempDir(),
		Seed: []string{badRoot},
	}}}
	_, err = b.initSkills(context.Background(), jail, nil)
	if err == nil {
		t.Fatal("initSkills = nil, want a fatal boot error for a plugins.seed refusal")
	}
	if !strings.Contains(err.Error(), "not linked") || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("boot error %q does not name the unlinked module", err)
	}
}

func findRowByName(rows []pluginreg.Plugin, name string) (pluginreg.Plugin, bool) {
	for _, p := range rows {
		if p.Name == name {
			return p, true
		}
	}
	return pluginreg.Plugin{}, false
}
