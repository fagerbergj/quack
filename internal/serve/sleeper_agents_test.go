package serve

import (
	"context"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// sleeperPluginRoot: resolved relative to this test's own cwd
// (internal/serve, go test's package-dir convention), not repo root or
// config/quack.yaml's own relative plugins.seed entry.
const sleeperPluginRoot = "../../.agents/plugins/sleeper"

// sleeperWorkflowShapeNames: the ten bound shapes .agents/plugins/sleeper/workflows/ declares.
var sleeperWorkflowShapeNames = []string{"sleeper-lineup", "sleeper-waivers", "sleeper-trends", "sleeper-season-notes", "sleeper-trade", "sleeper-trade-finder", "sleeper-digest", "sleeper-retro", "sleeper-draft", "sleeper-history"}

// resolveSleeperPlugin resolves the sleeper plugin root directly - the
// registry's own relative plugins.seed entries resolve against the SERVER's
// cwd, not a test binary's package-dir cwd, so tests bypass the registry.
func resolveSleeperPlugin(t *testing.T) plugin.Plugin {
	t.Helper()
	plugins, err := plugin.Resolve([]string{sleeperPluginRoot})
	if err != nil {
		t.Fatalf("plugin.Resolve(sleeper): %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugin.Resolve(sleeper) = %v, want exactly one plugin", plugins)
	}
	return plugins[0]
}

// enableSleeperExtension sets cfg.Extensions.Modules["sleeper"] to an
// enabled block, the same shape config.Load would produce from an
// uncommented extensions.sleeper: { enabled: true } in quack.yaml.
func enableSleeperExtension(t *testing.T, cfg *config.Config) {
	t.Helper()
	var node yaml.Node
	if err := node.Encode(map[string]any{"enabled": true, "default_user": "x", "default_league": "1"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Extensions.Modules == nil {
		cfg.Extensions.Modules = map[string]yaml.Node{}
	}
	cfg.Extensions.Modules["sleeper"] = node
}

// sleeperExtToolsByName builds the real sleeper extension the way
// buildOneSDKExtension does for an enabled config, indexed by tool name -
// the same map buildAgents hands tools.Build via ExtTools once enabled.
func sleeperExtToolsByName(t *testing.T) map[string]tool.Tool {
	t.Helper()
	factory, ok := extsdk.Registered()["sleeper"]
	if !ok {
		t.Fatal("sleeper extension not registered - see extensions_registry.go's blank import")
	}
	ext, err := factory(extsdk.Host{}, []byte("enabled: true\n"))
	if err != nil {
		t.Fatalf("sleeper factory: %v", err)
	}
	byName := map[string]tool.Tool{}
	for _, tl := range ext.Tools() {
		byName[tl.Name()] = tl
	}
	return byName
}

// TestSleeperPluginSeedsAgentsAndShapesWhenExtensionEnabled: with
// extensions.sleeper enabled, the plugin seeds exactly its seven agents and
// ten shapes, each agent's real tool set resolves, and every shape binds.
func TestSleeperPluginSeedsAgentsAndShapesWhenExtensionEnabled(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	enableSleeperExtension(t, cfg)
	p := resolveSleeperPlugin(t)

	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 1 || results[0].Plugin != "sleeper" {
		t.Fatalf("results = %+v, want one sleeper entry", results)
	}
	if got := results[0].Agents; len(got) != 7 {
		t.Errorf("seeded agents = %v, want exactly 7", got)
	}
	if got := results[0].Shapes; len(got) != 10 {
		t.Errorf("seeded shapes = %v, want exactly 10", got)
	}

	extToolsByName := sleeperExtToolsByName(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.NewJail: %v", err)
	}

	for _, name := range []string{"lineup-analyst", "waiver-scout", "trend-scout", "trade-analyst", "league-reporter", "history-analyst", "draft-analyst"} {
		ac, ok := cfg.Agents[name]
		if !ok {
			t.Fatalf("sleeper plugin did not seed agent %q", name)
		}
		if !ac.Optional {
			t.Errorf("agent %q: want optional: true - its tools need extensions.sleeper enabled to resolve", name)
		}
		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			t.Fatalf("agent %q: unknown provider %q", name, ac.Provider)
		}
		wm, err := inference.NewModel(prov, ac.Model, nil, cfg.ModelCost(ac.Model))
		if err != nil {
			t.Fatalf("agent %q: model: %v", name, err)
		}
		toolNames := resolveToolNames(ac.Tools, true)
		if _, err := tools.Build(toolNames, tools.Deps{
			WebSearch:       tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL, Key: cfg.Tools["web_search"].APIKey()},
			Fetch:           tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
			Summarizer:      wm,
			Workspace:       jail,
			WorkspaceUserID: "local",
			ExtTools:        extToolsByName,
		}); err != nil {
			t.Errorf("agent %q: tools did not resolve with extensions.sleeper enabled: %v", name, err)
		}
	}

	// None of the three agents dropped, so DropAgents is a no-op: every
	// sleeper-* shape stays in the catalog AND is bindable (has bound nodes).
	rawShapes := workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision)
	filtered := workflowcatalog.DropAgents(rawShapes, nil)
	for _, name := range sleeperWorkflowShapeNames {
		s, ok := workflowcatalog.Lookup(filtered, name)
		if !ok {
			t.Errorf("shape %q missing from the catalog with extensions.sleeper enabled", name)
			continue
		}
		if nodes, ok := workflowcatalog.Bind(s, "test ask"); !ok || len(nodes) == 0 {
			t.Errorf("shape %q not bindable (workflowcatalog.Bind found no nodes)", name)
		}
	}
}

// TestSleeperPluginAbsentWhenExtensionDisabled: with extensions.sleeper off
// (the shipped default), the gate keeps the plugin's agents and shapes OUT
// of cfg entirely - not seeded then dropped, simply never added.
func TestSleeperPluginAbsentWhenExtensionDisabled(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.LoadDeferringAgentCompleteness("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := resolveSleeperPlugin(t)

	results, err := SeedPluginAgentsAndShapes(cfg, []plugin.Plugin{p})
	if err != nil {
		t.Fatalf("SeedPluginAgentsAndShapes: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none with extensions.sleeper unconfigured", results)
	}
	for _, name := range []string{"lineup-analyst", "waiver-scout", "trend-scout", "trade-analyst", "league-reporter", "history-analyst", "draft-analyst"} {
		if _, ok := cfg.Agents[name]; ok {
			t.Errorf("agent %q seeded despite extensions.sleeper being unconfigured", name)
		}
	}

	rawShapes := workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision)
	for _, name := range sleeperWorkflowShapeNames {
		if _, ok := workflowcatalog.Lookup(rawShapes, name); ok {
			t.Errorf("shape %q present with extensions.sleeper unconfigured", name)
		}
	}
}

// TestSleeperAgentBundlesDeclareArtifactKinds locks the seven cards' declared
// default output kind to what internal/sleeperkinds registers - the
// agent.LoadBundle validation this whole PR exists to make real.
func TestSleeperAgentBundlesDeclareArtifactKinds(t *testing.T) {
	ctx := context.Background()
	want := map[string]string{
		"lineup-analyst":  "lineup",
		"waiver-scout":    "waivers",
		"trend-scout":     "trends",
		"trade-analyst":   "trade",
		"league-reporter": "digest",
		"history-analyst": "history",
		"draft-analyst":   "draft",
	}
	for name, kind := range want {
		b, err := agent.LoadBundle(ctx, nil, sleeperPluginRoot+"/agents/"+name)
		if err != nil {
			t.Fatalf("LoadBundle(%s): %v", name, err)
		}
		if b.Card.Artifact != kind {
			t.Errorf("%s: card.Artifact = %q, want %q", name, b.Card.Artifact, kind)
		}
	}
}
