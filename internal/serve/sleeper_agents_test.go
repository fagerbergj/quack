package serve

import (
	"os"
	"regexp"
	"slices"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"google.golang.org/adk/v2/tool"
	"gopkg.in/yaml.v3"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// sleeperPluginRoot is a fixture shaped like quack-extensions' sleeper/plugin,
// which ships and tests the real agents; quack only proves the seam here.
const sleeperPluginRoot = "testdata/plugins/sleeper"

var (
	sleeperWorkflowShapeNames = []string{"sleeper-fixture"}
	sleeperAgentNames         = []string{"fixture-analyst"}
)

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

// newSleeperExtension builds the real sleeper extension the way
// buildOneSDKExtension does for an enabled config.
func newSleeperExtension(t *testing.T) extsdk.Extension {
	t.Helper()
	factory, ok := extsdk.Registered()["sleeper"]
	if !ok {
		t.Fatal("sleeper extension not registered - see extensions_registry.go's blank import")
	}
	ext, err := factory(extsdk.Host{}, []byte("enabled: true\n"))
	if err != nil {
		t.Fatalf("sleeper factory: %v", err)
	}
	return ext
}

// sleeperExtToolsByName indexes the extension's tools by name - the map
// buildAgents hands tools.Build via ExtTools once enabled.
func sleeperExtToolsByName(t *testing.T) map[string]tool.Tool {
	t.Helper()
	byName := map[string]tool.Tool{}
	for _, tl := range newSleeperExtension(t).Tools() {
		byName[tl.Name()] = tl
	}
	return byName
}

// TestSleeperPluginSeedsAgentsAndShapesWhenExtensionEnabled: with
// extensions.sleeper enabled, the plugin seeds its agents and shapes, each
// agent's sleeper tools resolve against the linked extension, and every shape binds.
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
	if got := results[0].Agents; !slices.Equal(got, sleeperAgentNames) {
		t.Errorf("seeded agents = %v, want %v", got, sleeperAgentNames)
	}
	if got := results[0].Shapes; !slices.Equal(got, sleeperWorkflowShapeNames) {
		t.Errorf("seeded shapes = %v, want %v", got, sleeperWorkflowShapeNames)
	}

	extToolsByName := sleeperExtToolsByName(t)
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.NewJail: %v", err)
	}

	for _, name := range sleeperAgentNames {
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

	// No agent dropped, so DropAgents is a no-op: every shape stays in the
	// catalog AND is bindable (has bound nodes).
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
	for _, name := range sleeperAgentNames {
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

// TestSleeperArtifactSchemasAreRegisteredKinds: the plugin's cards name the
// kinds the extension declares schemas for, so each must be a registered artifact kind.
func TestSleeperArtifactSchemasAreRegisteredKinds(t *testing.T) {
	decl, ok := newSleeperExtension(t).(extsdk.ArtifactSchemas)
	if !ok {
		t.Fatal("sleeper extension no longer declares ArtifactSchemas")
	}
	for kind := range decl.ArtifactSchemas() {
		if err := recordstore.ValidateArtifactKind(kind); err != nil {
			t.Errorf("sleeper schema kind %q: %v (add it to internal/sleeperkinds)", kind, err)
		}
	}
}

// One sleeper/vX.Y.Z tag releases both the Go tools and the plugin; pinned
// apart, a plugin agent can name a tool this binary lacks and is dropped.
func TestSleeperPluginSeedMatchesGoModPin(t *testing.T) {
	read := func(path string, re *regexp.Regexp) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: no match for %s", path, re)
		}
		return string(m[1])
	}
	seed := read("../../config/quack.yaml", regexp.MustCompile(`github:fagerbergj/quack-extensions@sleeper/(v\S+)#sleeper/plugin`))
	mod := read("../../go.mod", regexp.MustCompile(`github\.com/fagerbergj/quack-extensions/sleeper (v\S+)`))
	if seed != mod {
		t.Fatalf("config/quack.yaml seeds the sleeper plugin at %s but go.mod pins sleeper %s; bump both to one tag", seed, mod)
	}
}
