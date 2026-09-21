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
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

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

	// Wired exactly as boot() wires it: builtinSkillSrc is WrapRef over
	// shapesRef, so it (and everything built from it below, including
	// newScopedSkillTS - the orchestrator's own plan-work path) live-reflects
	// whatever finalizeCatalogShapes later Stores.
	rawShapes := workflowcatalog.FromConfig(cfg.Workflows, cfg.Revision)
	var shapesRef atomic.Pointer[[]workflowcatalog.Shape]
	shapesRef.Store(&rawShapes)
	swappable := newSwappableSkillSource(newSkillSource(nil))
	builtinSkillSrc := workflowcatalog.WrapRef(skill.Source(swappable), &shapesRef)
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		t.Fatalf("skill toolset: %v", err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}
	// planWorkInstructions mirrors what assembleOrchestrator actually reads
	// (newScopedSkillTS(cfg.Orchestrator.Skills), serve.go) - the real path
	// round 2 found the old test never exercised.
	planWorkInstructions := func() string {
		t.Helper()
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, []string{"plan-work"}), jail, localUserID)
		got, err := src.LoadInstructions(context.Background(), "plan-work")
		if err != nil {
			t.Fatalf("LoadInstructions(plan-work): %v", err)
		}
		return got
	}
	if before := planWorkInstructions(); !strings.Contains(before, "resolves trigger") || !strings.Contains(before, "drops trigger") {
		t.Fatalf("both triggers must render before buildAgents/finalizeCatalogShapes run:\n%s", before)
	}

	var setupFn dag.SetupFunc
	artifacts := artifact.InMemoryService()
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, res, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifacts, nil, nil, nil, nil)
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

	// finalizeCatalogShapes (serve.go) must remove "drops-shape" - it names
	// the dropped agent - from the REAL rendered plan-work table, and log why,
	// while "resolves-shape" (names the agent that built fine) survives.
	var buf bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLog)

	b := &boot{cfg: cfg}
	b.finalizeCatalogShapes(rawShapes, clientMap, &shapesRef)

	after := planWorkInstructions()
	if strings.Contains(after, "drops trigger") {
		t.Errorf(`"drops trigger" still renders in the orchestrator's own plan-work instructions after filtering:\n%s`, after)
	}
	if !strings.Contains(after, "resolves trigger") {
		t.Errorf(`"resolves trigger" is missing after filtering - a surviving shape must stay rendered:\n%s`, after)
	}
	if !strings.Contains(buf.String(), "shape names a dropped optional agent") || !strings.Contains(buf.String(), "drops-shape") {
		t.Errorf("expected a warning naming the dropped shape, got:\n%s", buf.String())
	}
}
