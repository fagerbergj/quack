package serve

import (
	"context"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// sleeperWorkflowShapeNames: the four bound shapes config/quack.yaml's
// workflows: declares for the Sleeper extension's dispatch (docs/configuration/agents.md).
var sleeperWorkflowShapeNames = []string{"sleeper-lineup", "sleeper-waivers", "sleeper-trends", "sleeper-season-notes"}

// sleeperExtToolsByName builds the real sleeper extension the way
// buildOneSDKExtension does for an enabled config, and indexes its Tools() by
// name - the same map buildAgents would hand tools.Build via ExtTools once
// extensions.sleeper is on.
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

// TestSleeperAgentsResolveToolsWhenExtensionEnabled mirrors
// nativeAgentGitHubWriteGrants (stagedeliver_test.go): build each Sleeper
// agent's real tool set against the real, enabled extension's ExtTools, the
// way buildAgents does, and confirm every declared tool actually resolves -
// config.Load parsing the tools: list isn't enough, tools.Build is the real gate.
func TestSleeperAgentsResolveToolsWhenExtensionEnabled(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	extToolsByName := sleeperExtToolsByName(t)

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.NewJail: %v", err)
	}

	for _, name := range []string{"lineup-analyst", "waiver-scout", "trend-scout"} {
		ac, ok := cfg.Agents[name]
		if !ok {
			t.Fatalf("config/quack.yaml missing agent %q", name)
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
		toolNames := resolveToolNames(ac.Tools, true, true)
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
			t.Errorf("shape %q missing from config/quack.yaml's workflows: with extensions.sleeper enabled", name)
			continue
		}
		if nodes, ok := workflowcatalog.Bind(s, "test ask"); !ok || len(nodes) == 0 {
			t.Errorf("shape %q not bindable (workflowcatalog.Bind found no nodes)", name)
		}
	}
}

// TestSleeperWorkflowShapesAbsentWhenExtensionDisabled is the boot test's
// other half: with extensions.sleeper off (the shipped default), the three
// Sleeper agents' tools never resolve, buildAgents drops them, and
// finalizeCatalogShapes' DropAgents must remove every shape naming one -
// a planner or extension dispatch must never see a job it can't run.
func TestSleeperWorkflowShapesAbsentWhenExtensionDisabled(t *testing.T) {
	requireStageDeliverEnv(t)
	cfg, err := config.Load("../../config/quack.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.NewJail: %v", err)
	}

	dropped := map[string]bool{}
	for _, name := range []string{"lineup-analyst", "waiver-scout", "trend-scout"} {
		ac := cfg.Agents[name]
		prov, ok := cfg.Provider(ac.Provider)
		if !ok {
			t.Fatalf("agent %q: unknown provider %q", name, ac.Provider)
		}
		wm, err := inference.NewModel(prov, ac.Model, nil, cfg.ModelCost(ac.Model))
		if err != nil {
			t.Fatalf("agent %q: model: %v", name, err)
		}
		toolNames := resolveToolNames(ac.Tools, true, true)
		// No ExtTools: extensions.sleeper is off by default, exactly as a
		// fresh clone boots - sleeper_* never resolves.
		if _, err := tools.Build(toolNames, tools.Deps{
			WebSearch:       tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL, Key: cfg.Tools["web_search"].APIKey()},
			Fetch:           tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
			Summarizer:      wm,
			Workspace:       jail,
			WorkspaceUserID: "local",
		}); err != nil {
			if !ac.Optional {
				t.Fatalf("agent %q: tools failed to resolve and it isn't optional: %v", name, err)
			}
			dropped[name] = true
			continue
		}
		t.Errorf("agent %q: tools resolved with extensions.sleeper disabled - test fixture drifted from the real config", name)
	}
	if len(dropped) != 3 {
		t.Fatalf("want all three Sleeper agents dropped with extensions.sleeper disabled, got %v", dropped)
	}

	rawShapes := workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision)
	filtered := workflowcatalog.DropAgents(rawShapes, dropped)
	for _, name := range sleeperWorkflowShapeNames {
		if _, ok := workflowcatalog.Lookup(filtered, name); ok {
			t.Errorf("shape %q present after dropping the Sleeper agents; DropAgents should have removed it", name)
		}
	}
}

// TestSleeperAgentBundlesDeclareArtifactKinds locks the three cards' declared
// default output kind to what internal/sleeperkinds registers - the
// agent.LoadBundle validation this whole PR exists to make real.
func TestSleeperAgentBundlesDeclareArtifactKinds(t *testing.T) {
	ctx := context.Background()
	want := map[string]string{"lineup-analyst": "lineup", "waiver-scout": "waivers", "trend-scout": "trends"}
	for name, kind := range want {
		b, err := agent.LoadBundle(ctx, nil, "agents/"+name)
		if err != nil {
			t.Fatalf("LoadBundle(%s): %v", name, err)
		}
		if b.Card.Artifact != kind {
			t.Errorf("%s: card.Artifact = %q, want %q", name, b.Card.Artifact, kind)
		}
	}
}
