package serve

import (
	"context"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/skillsource"
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
}
