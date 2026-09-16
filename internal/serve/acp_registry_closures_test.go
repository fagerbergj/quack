package serve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/pluginreg"
)

// TestAcpRegistryExtraROGrantsRootOnlyWhenItExists: the sandbox grant
// (never fed into skill_paths, #1430) tracks whether plugins.root actually
// exists on disk - nothing to grant before the first plugin is fetched.
func TestAcpRegistryExtraROGrantsRootOnlyWhenItExists(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	cfg := &config.Config{Plugins: &config.PluginsConfig{Root: root}}
	fn := acpRegistryExtraRO(cfg)

	if got := fn(); got != nil {
		t.Fatalf("ExtraRO before root exists = %v, want nil", got)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := fn(); len(got) != 1 || got[0] != root {
		t.Fatalf("ExtraRO once root exists = %v, want [%q]", got, root)
	}
}

// TestAcpRegistrySkillPathsResolvesAndCaches: a fresh registry read resolves
// real skill dirs, and a second call with an unchanged registry hits the
// registrySignature cache (same slice, not a fresh resolve every spawn).
func TestAcpRegistrySkillPathsResolvesAndCaches(t *testing.T) {
	root := t.TempDir()
	pluginRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pluginRoot, "plugin.json"), []byte(`{"name":"extra"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeVendorSkill(t, filepath.Join(pluginRoot, "skills"), "widget-maker", "Makes widgets.")
	entry, err := pluginreg.ParseEntry(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	reg := pluginreg.NewFSRegistry(root)
	if err := reg.Put(t.Context(), pluginreg.FromEntry(entry)); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Plugins: &config.PluginsConfig{Root: root}}
	fn := acpRegistrySkillPaths(cfg, reg)

	got := fn()
	if len(got) == 0 {
		t.Fatalf("acpRegistrySkillPaths = %v, want the resolved plugin's skills dir", got)
	}
	wantDir := filepath.Join(pluginRoot, "skills")
	found := false
	for _, p := range got {
		if p == wantDir {
			found = true
		}
	}
	if !found {
		t.Errorf("paths = %v, want %q among them", got, wantDir)
	}

	// Same registry state: the cached slice comes back (registrySignature unchanged).
	second := fn()
	if len(second) != len(got) {
		t.Errorf("second call = %v, want the same cached result %v", second, got)
	}
}
