package serve

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestBuildAgentsDropsOptionalAgentOnUnresolvedTools proves buildAgents'
// degrade-honestly path (serve.go): an agent marked optional: true whose
// tools don't resolve (its extension is off, here simulated with a made-up
// tool name) is dropped from the roster with a warning, not a boot error -
// while a normal, resolvable agent still builds.
func TestBuildAgentsDropsOptionalAgentOnUnresolvedTools(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	builtinSkillSrc := newSkillSource(nil)
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		t.Fatalf("skill toolset: %v", err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}
	res := artifactsrc.New("stub", &stubBindingSource{}, time.Nanosecond)

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"stub-test": {Kind: "openai", Endpoint: "http://fake-provider.invalid"}},
		Models:    map[string]config.ModelConfig{"any-model": {Provider: "stub-test"}},
		Agents: map[string]config.AgentConfig{
			"tester": {Bundle: "agents/web-researcher", Provider: "stub-test", Model: "any-model"},
			"broken-optional": {
				Bundle: "agents/web-researcher", Provider: "stub-test", Model: "any-model",
				Optional: true, Tools: []string{"definitely_not_a_real_tool"},
			},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
		Workflows: []config.WorkflowShape{
			{
				Name: "resolves-shape", Trigger: "resolves trigger", Shape: "s", Agents: []string{"tester"},
				Nodes: []config.WorkflowNode{{ID: "n1", Agent: "tester", Task: "do it"}},
			},
			{
				Name: "drops-shape", Trigger: "drops trigger", Shape: "s", Agents: []string{"broken-optional"},
				Nodes: []config.WorkflowNode{{ID: "n1", Agent: "broken-optional", Task: "do it"}},
			},
		},
	}

	var setupFn dag.SetupFunc
	artifacts := artifact.InMemoryService()
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, res, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifacts, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildAgents: %v (an optional agent's unresolved tools must not fail boot)", err)
	}
	defer nodeServers.closeAll()

	if _, ok := clientMap["tester"]; !ok {
		t.Error(`clientMap["tester"] missing - a normal agent must still build`)
	}
	if _, ok := clientMap["broken-optional"]; ok {
		t.Error(`clientMap["broken-optional"] present - an optional agent with unresolved tools must be dropped`)
	}

	// finalizeCatalogShapes (serve.go) must remove "drops-shape" - it names the
	// dropped agent - from BOTH catalog consumers, and log why, while
	// "resolves-shape" (names the agent that built fine) survives.
	var buf bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLog)

	rawShapes := workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision)
	var shapesRef atomic.Pointer[[]workflowcatalog.Shape]
	shapesRef.Store(&rawShapes)
	swappable := newSwappableSkillSource(builtinSkillSrc)
	b := &boot{cfg: cfg}
	planSkillSrc := b.finalizeCatalogShapes(rawShapes, clientMap, &shapesRef, swappable, jail)
	if planSkillSrc == nil {
		t.Fatal("finalizeCatalogShapes returned a nil skill source")
	}

	filtered := *shapesRef.Load()
	if _, ok := workflowcatalog.Lookup(filtered, "drops-shape"); ok {
		t.Error(`shapesRef still names "drops-shape" - its agent was dropped, the extension dispatch catalog must not offer it`)
	}
	if _, ok := workflowcatalog.Lookup(filtered, "resolves-shape"); !ok {
		t.Error(`shapesRef lost "resolves-shape" - its agent built fine, it must stay bindable`)
	}
	if !strings.Contains(buf.String(), "shape names a dropped optional agent") || !strings.Contains(buf.String(), "drops-shape") {
		t.Errorf("expected a warning naming the dropped shape, got:\n%s", buf.String())
	}
}
