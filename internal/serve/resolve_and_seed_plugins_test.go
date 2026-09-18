package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/config"
)

// bootPluginRegistry's own open failure (an unknown plugins.store name)
// propagates through resolvePlugins.
func TestResolvePlugins_RegistryOpenErrorPropagates(t *testing.T) {
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: t.TempDir(), Store: "does-not-exist"}}}
	if _, _, _, err := b.resolvePlugins(context.Background(), nil); err == nil {
		t.Fatal("expected an error for an unknown plugins.store")
	}
	if _, _, _, err := b.resolveAndSeedPlugins(context.Background(), nil); err == nil {
		t.Fatal("resolveAndSeedPlugins must propagate the same registry-open error")
	}
}

// A malformed extensions[Namespace] block (unsupported schemaVersion) makes
// plugin.Resolve fail outright, which resolveRegistryPlugins/resolvePlugins propagate.
func TestResolvePlugins_MalformedNamespaceBlockPropagates(t *testing.T) {
	root := t.TempDir()
	body := `{"$schema":"x","name":"bad","extensions":{"io.github.fagerbergj.quack":{"schemaVersion":2}}}`
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &boot{cfg: &config.Config{Plugins: &config.PluginsConfig{Root: t.TempDir(), Seed: []string{root}}}}
	if _, _, _, err := b.resolvePlugins(context.Background(), nil); err == nil {
		t.Fatal("expected an error for an unsupported schemaVersion")
	}
}

// resolveAndSeedPlugins' happy path: a local plugin's agents seed into
// b.cfg and the returned shapes match workflowcatalog.FromConfig(b.cfg.Workflows).
func TestResolveAndSeedPlugins_Success(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{"$schema":"x","name":"acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGenericAgentBundle(t, filepath.Join(root, "agents"), "scout")

	cfg := minimalPluginTestConfig()
	cfg.Agents["scout"] = config.AgentConfig{Model: "m"}
	cfg.Plugins = &config.PluginsConfig{Root: t.TempDir(), Seed: []string{root}}
	b := &boot{cfg: cfg}

	reg, plugins, shapes, err := b.resolveAndSeedPlugins(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolveAndSeedPlugins: %v", err)
	}
	if reg == nil || len(plugins) == 0 {
		t.Fatalf("reg/plugins = %v/%v, want a resolved registry and at least one plugin", reg, plugins)
	}
	if len(shapes) != len(cfg.Workflows) {
		t.Errorf("shapes = %d, want len(cfg.Workflows) = %d", len(shapes), len(cfg.Workflows))
	}
	if _, ok := cfg.Agents["scout"]; !ok {
		t.Error("scout not seeded into cfg.Agents")
	}
}

// resolveAndSeedPlugins propagates a SeedPluginAgentsAndShapes failure
// (here: a malformed agent.yaml) instead of swallowing it.
func TestResolveAndSeedPlugins_SeedErrorPropagates(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{"$schema":"x","name":"acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(root, "agents")
	writeGenericAgentBundle(t, agentsDir, "scout")
	if err := os.WriteFile(filepath.Join(agentsDir, "scout", "agent.yaml"), []byte("tools: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := minimalPluginTestConfig()
	cfg.Plugins = &config.PluginsConfig{Root: t.TempDir(), Seed: []string{root}}
	b := &boot{cfg: cfg}
	if _, _, _, err := b.resolveAndSeedPlugins(context.Background(), nil); err == nil {
		t.Fatal("expected the malformed agent.yaml error to propagate")
	}
}
