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

// TestBuildAgentsDropsOptionalAgentOnUnresolvedTools: an optional agent whose tools don't resolve is
// dropped with a warning, not a boot error, while a resolvable agent still builds.
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

	// Wired as boot() does: builtinSkillSrc wraps shapesRef, so everything built from it (including
	// newScopedSkillTS's plan-work path) reflects whatever catalogShapes later stores.
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
	// planWorkInstructions reads what assembleOrchestrator reads:
	// newScopedSkillTS(cfg.Orchestrator.Skills).
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
		t.Fatalf("both triggers must render before buildAgents/catalogShapes run:\n%s", before)
	}

	var setupFn dag.SetupFunc
	artifacts := artifact.InMemoryService()
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, res, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifacts, nil, nil, nil, nil, newPerNodeServers(), nil)
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

	// catalogShapes must remove "drops-shape" (it names the dropped agent) from the rendered plan-work
	// table and log why, while "resolves-shape" survives.
	var buf bytes.Buffer
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prevLog)

	filtered := catalogShapes(rawShapes, clientMap)
	shapesRef.Store(&filtered)

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
