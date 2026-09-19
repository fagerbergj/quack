package serve

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
)

func minimalPluginTestConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{"default": {Kind: "openai", Endpoint: "http://localhost:1"}},
		Models:    map[string]config.ModelConfig{"m": {Provider: "default", Role: "worker"}},
		Agents:    map[string]config.AgentConfig{},
	}
}

func writeGenericAgentBundle(t *testing.T, agentsDir, name string) {
	t.Helper()
	dir := filepath.Join(agentsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-card.json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A plugin naming no linked module seeds unconditionally - config's
// extensions: block never enters into it.
func TestPluginGateEnabled_NoModuleSeedsUnconditionally(t *testing.T) {
	cfg := minimalPluginTestConfig()
	enabled, err := pluginGateEnabled(cfg, plugin.Plugin{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Error("a plugin with no declared module must seed unconditionally")
	}
}

// A plugin naming a module absent from extensions: is gated off, not warned
// about - absence, not a drop.
func TestPluginGateEnabled_ModuleUnconfiguredGatesOff(t *testing.T) {
	cfg := minimalPluginTestConfig()
	p := plugin.Plugin{Name: "acme", Modules: []plugin.Module{{Name: "acme", Path: "x"}}}
	enabled, err := pluginGateEnabled(cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("a plugin whose module is absent from extensions: must gate off")
	}
}

// extensions.<module>: { enabled: false } gates the plugin off exactly like
// buildOneSDKExtension keeps the module itself dormant.
func TestPluginGateEnabled_ModuleDisabledGatesOff(t *testing.T) {
	cfg := minimalPluginTestConfig()
	cfg.Extensions.Modules = map[string]yaml.Node{"acme": moduleNode(t, "enabled: false")}
	p := plugin.Plugin{Name: "acme", Modules: []plugin.Module{{Name: "acme", Path: "x"}}}
	enabled, err := pluginGateEnabled(cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("extensions.acme.enabled: false must gate the plugin off")
	}
}

// extensions.<module>: present and enabled (or Enabled unset, which defaults
// true) gates the plugin on.
func TestPluginGateEnabled_ModuleEnabledSeeds(t *testing.T) {
	cfg := minimalPluginTestConfig()
	cfg.Extensions.Modules = map[string]yaml.Node{"acme": moduleNode(t, "enabled: true")}
	p := plugin.Plugin{Name: "acme", Modules: []plugin.Module{{Name: "acme", Path: "x"}}}
	enabled, err := pluginGateEnabled(cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Error("extensions.acme.enabled: true must gate the plugin on")
	}
}

// SeedPluginAgentsAndShapes end to end: a gated-off plugin contributes
// nothing and is absent from the returned results (no log-worthy entry).
func TestSeedPluginAgentsAndShapes_GatedOffContributesNothing(t *testing.T) {
	cfg := minimalPluginTestConfig()
	agentsDir := t.TempDir()
	writeGenericAgentBundle(t, agentsDir, "scout")
	p := plugin.Plugin{Name: "acme", AgentsDir: agentsDir, Modules: []plugin.Module{{Name: "acme", Path: "x"}}}

	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want none", results)
	}
	if _, ok := cfg.Agents["scout"]; ok {
		t.Error("agent seeded despite the plugin's module being unconfigured")
	}
}

// A plugin with neither agents/ nor workflows/ (skills-only, e.g. usage)
// is skipped outright - never even reaches the gate check.
func TestSeedPluginAgentsAndShapes_SkillsOnlyPluginContributesNothing(t *testing.T) {
	cfg := minimalPluginTestConfig()
	p := plugin.Plugin{Name: "usage", SkillsDir: "/does/not/matter"}
	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want none for a skills-only plugin", results)
	}
}

// A plugin with no declared module, agents seeded, deployment override
// supplying the model (model_role left unset in this fixture) - proves the
// unconditional-seed path reaches c.Agents through SeedPluginAgentsAndShapes.
func TestSeedPluginAgentsAndShapes_UnconditionalPluginSeeds(t *testing.T) {
	cfg := minimalPluginTestConfig()
	cfg.Agents["scout"] = config.AgentConfig{Model: "m"}
	agentsDir := t.TempDir()
	writeGenericAgentBundle(t, agentsDir, "scout")
	p := plugin.Plugin{Name: "acme", AgentsDir: agentsDir, Agents: []string{"scout"}}

	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 1 || len(results[0].Agents) != 1 || results[0].Agents[0] != "scout" {
		t.Fatalf("results = %+v, want one acme entry seeding scout", results)
	}
	if ac := cfg.Agents["scout"]; ac.Model != "m" || !ac.Optional {
		t.Errorf("scout = %+v, want model m and optional true", ac)
	}
}

// A malformed extensions.<module> block (enabled: isn't a bool) makes
// moduleEnabled error, which pluginGateEnabled and SeedPluginAgentsAndShapes
// both propagate rather than silently treating as disabled.
func TestPluginGateEnabled_MalformedModuleBlockErrors(t *testing.T) {
	cfg := minimalPluginTestConfig()
	cfg.Extensions.Modules = map[string]yaml.Node{"acme": moduleNode(t, "enabled: not-a-bool")}
	p := plugin.Plugin{Name: "acme", Modules: []plugin.Module{{Name: "acme", Path: "x"}}}
	if _, err := pluginGateEnabled(cfg, p); err == nil {
		t.Fatal("expected an error for a malformed extensions.acme block")
	}
	if _, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{{Name: "acme", AgentsDir: t.TempDir(), Modules: p.Modules}}); err == nil {
		t.Fatal("SeedPluginAgentsAndShapes must propagate the gate error")
	}
}

// SeedPluginAgentsAndShapes propagates a failing agent bundle (malformed
// agent.yaml) rather than skipping it silently.
func TestSeedPluginAgentsAndShapes_AgentSeedErrorPropagates(t *testing.T) {
	cfg := minimalPluginTestConfig()
	agentsDir := t.TempDir()
	writeGenericAgentBundle(t, agentsDir, "scout")
	if err := os.WriteFile(filepath.Join(agentsDir, "scout", "agent.yaml"), []byte("tools: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := plugin.Plugin{Name: "acme", AgentsDir: agentsDir, Agents: []string{"scout"}}
	if _, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p}); err == nil {
		t.Fatal("expected the malformed agent.yaml error to propagate")
	}
}

// SeedPluginAgentsAndShapes propagates a failing workflow shape (names an
// unconfigured agent) rather than skipping it silently.
func TestSeedPluginAgentsAndShapes_ShapeSeedErrorPropagates(t *testing.T) {
	cfg := minimalPluginTestConfig()
	workflowsDir := t.TempDir()
	shape := "name: acme-job\ntrigger: \"Run the acme job\"\nagents: [ghost]\nshape: \"ONE `ghost` node\"\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "acme-job.yaml"), []byte(shape), 0o644); err != nil {
		t.Fatal(err)
	}
	p := plugin.Plugin{Name: "acme", WorkflowsDir: workflowsDir, Workflows: []string{"acme-job"}}
	if _, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p}); err == nil {
		t.Fatal("expected the missing-agent shape error to propagate")
	}
}

// ResolveConfiguredPlugins resolves a config-only local plugin root with no
// registry store, session, or server startup involved.
func TestResolveConfiguredPlugins_LocalRoot(t *testing.T) {
	// A local root's registry name is its directory's base name, not
	// plugin.json's own "name" - so this dir must be named "acme".
	root := filepath.Join(t.TempDir(), "acme")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{"$schema":"x","name":"acme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := minimalPluginTestConfig()
	cfg.Plugins = &config.PluginsConfig{Root: t.TempDir(), Seed: []string{root}}

	plugins, unresolvable, err := ResolveConfiguredPlugins(cfg)
	if err != nil {
		t.Fatalf("ResolveConfiguredPlugins: %v", err)
	}
	if len(unresolvable) != 0 {
		t.Errorf("unresolvable = %v, want none for a local root", unresolvable)
	}
	found := false
	for _, p := range plugins {
		if p.Name == "acme" {
			found = true
		}
	}
	if !found {
		t.Errorf("plugins = %+v, want the local acme root resolved", plugins)
	}
}

// A github: seed entry with no local clone yet is reported unresolvable and
// skipped, never fetched - ResolveConfiguredPlugins never touches the network.
func TestResolveConfiguredPlugins_UnclonedGitHubEntryUnresolvable(t *testing.T) {
	cfg := minimalPluginTestConfig()
	registryRoot := t.TempDir()
	cfg.Plugins = &config.PluginsConfig{Root: registryRoot, Seed: []string{"github:fagerbergj/dotagents"}}

	plugins, unresolvable, err := ResolveConfiguredPlugins(cfg)
	if err != nil {
		t.Fatalf("ResolveConfiguredPlugins: %v", err)
	}
	if len(plugins) != 0 {
		t.Errorf("plugins = %+v, want none (never cloned)", plugins)
	}
	if len(unresolvable) != 1 || unresolvable[0] != "dotagents" {
		t.Fatalf("unresolvable = %v, want [dotagents]", unresolvable)
	}
	entries, err := os.ReadDir(registryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("registryRoot has %d entries, want 0 - ResolveConfiguredPlugins must never write or fetch", len(entries))
	}
}

func TestLogPluginSeeds_DoesNotPanic(t *testing.T) {
	logPluginSeeds([]PluginSeedResult{{Plugin: "acme", Agents: []string{"scout"}, Shapes: []string{"acme-job"}}})
}
