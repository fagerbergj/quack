package serve

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/plugin"
)

// githubPluginRoot: resolved relative to this test's own cwd (internal/serve),
// not config/quack.yaml's own relative plugins.seed entry - see sleeperPluginRoot.
const githubPluginRoot = "../../.agents/plugins/github"

// githubCodeAgentNames are the three bundles .agents/plugins/github/plugin.json lists.
var githubCodeAgentNames = []string{"code-implementer", "code-reviewer", "code-explorer"}

func resolveGithubPlugin(t *testing.T) plugin.Plugin {
	t.Helper()
	plugins, err := plugin.Resolve([]string{githubPluginRoot})
	if err != nil {
		t.Fatalf("plugin.Resolve(github): %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugin.Resolve(github) = %v, want exactly one plugin", plugins)
	}
	return plugins[0]
}

// enableGithubExtension sets cfg.Extensions.Modules["github"] to an enabled
// block - the seeding gate only checks this node's presence/enabled flag
// (moduleEnabledIn), never the extension's own App-secret validation.
func enableGithubExtension(t *testing.T, cfg *config.Config) {
	t.Helper()
	var node yaml.Node
	if err := node.Encode(map[string]any{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Extensions.Modules == nil {
		cfg.Extensions.Modules = map[string]yaml.Node{}
	}
	cfg.Extensions.Modules["github"] = node
}

// TestGithubPluginSeedsAgentsWhenExtensionEnabled: with extensions.github
// enabled, the plugin seeds exactly its three agents, each merging config/quack.yaml's
// own override (memory bucket + acp block) onto the plugin's bundle/model_role/judge_rounds defaults.
func TestGithubPluginSeedsAgentsWhenExtensionEnabled(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	enableGithubExtension(t, cfg)
	p := resolveGithubPlugin(t)

	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 1 || results[0].Plugin != "github" {
		t.Fatalf("results = %+v, want one github entry", results)
	}
	if got := results[0].Agents; len(got) != 3 {
		t.Errorf("seeded agents = %v, want exactly 3", got)
	}

	for _, name := range githubCodeAgentNames {
		ac, ok := cfg.Agents[name]
		if !ok {
			t.Fatalf("github plugin did not seed agent %q", name)
		}
		if !ac.Optional {
			t.Errorf("agent %q: want optional: true - a disabled extension must drop it, not fail boot", name)
		}
		if ac.Bundle == "" {
			t.Errorf("agent %q: bundle not filled in by the plugin", name)
		}
		if ac.Memory.Bucket != "coding" {
			t.Errorf("agent %q: memory.bucket = %q, want %q (config/quack.yaml's own override)", name, ac.Memory.Bucket, "coding")
		}
		if ac.Acp == nil || len(ac.Acp.Command) == 0 {
			t.Errorf("agent %q: acp block not carried through from config/quack.yaml's override", name)
		}
		if _, err := agent.LoadBundle(context.Background(), nil, ac.Bundle); err != nil {
			t.Errorf("agent %q: bundle %q failed to load: %v", name, ac.Bundle, err)
		}
	}

	if err := cfg.RequireAgentBundlesAndModels(); err != nil {
		t.Errorf("RequireAgentBundlesAndModels: %v", err)
	}
}

// TestGithubPluginAbsentWhenExtensionDisabled: with extensions.github off
// (the shipped default), the three override-only entries are dropped from
// cfg.Agents entirely - not seeded then dropped, never filled in and pruned - and the config still passes RequireAgentBundlesAndModels.
func TestGithubPluginAbsentWhenExtensionDisabled(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := resolveGithubPlugin(t)

	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none with extensions.github unconfigured", results)
	}
	for _, name := range githubCodeAgentNames {
		if _, ok := cfg.Agents[name]; ok {
			t.Errorf("agent %q present despite extensions.github being unconfigured", name)
		}
	}

	if err := cfg.RequireAgentBundlesAndModels(); err != nil {
		t.Errorf("RequireAgentBundlesAndModels: %v (a disabled extension must still boot clean)", err)
	}
}
