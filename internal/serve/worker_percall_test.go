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
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestWorkerModelHoldsPerCall proves #1482's wiring: the model the served
// worker agent holds is the per-call admitting wrap, so a worker's slot
// frees between model calls and tool phases overlap other nodes' runs.
func TestWorkerModelHoldsPerCall(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	builtinSkillSrc := newSkillSource(nil)
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		t.Fatalf("skill tool set: %v", err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"stub-test": {Kind: "openai", Endpoint: "http://fake-provider.invalid"},
		},
		Models: map[string]config.ModelConfig{
			"any-model": {Provider: "stub-test", Limits: &config.ModelLimits{Sessions: 1}},
		},
		Agents: map[string]config.AgentConfig{
			"tester": {Bundle: "agents/web-researcher", Provider: "stub-test", Model: "any-model"},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}

	admission := dag.NewAdmission(nil, nil, nil, 0)
	var setupFn dag.SetupFunc
	res := artifactsrc.New("stub", &stubBindingSource{}, time.Nanosecond)
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, res, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifact.InMemoryService(), nil, nil, admission)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	na, ok := clientMap["tester"].(nativeAgent)
	if !ok {
		t.Fatalf("clientMap[%q] = %T, want nativeAgent", "tester", clientMap["tester"])
	}
	_, wm, _, _, _, release, err := na.ForNode("test-plan:test-node", nil, artifact.InMemoryService(), "quack-test", "u1", "chat-1", "test-node", nil)
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release(false)

	// The served agent's model must be the per-call admitting wrap, with the
	// overridable inside it (so a round-bound swap stays admitted).
	adm, ok := wm.(*dag.AdmittingLLM)
	if !ok {
		t.Fatalf("worker model = %T, want *dag.AdmittingLLM - the served agent's model must admit per call", wm)
	}
	if _, ok := adm.LLM.(*inference.OverridableModel); !ok {
		t.Fatalf("admitting target = %T, want *inference.OverridableModel", adm.LLM)
	}
}
