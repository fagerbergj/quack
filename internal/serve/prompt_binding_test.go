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

// stubBindingSource resolves system/tester to whatever Config the test sets,
// re-fetched every call (New's ttl below is effectively zero).
type stubBindingSource struct{ cfg map[string]any }

func (s *stubBindingSource) Get(_ context.Context, name string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{Body: "prompt body", Config: s.cfg, VersionID: "v1"}, true, nil
}
func (s *stubBindingSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// TestPromptBindingOverridesWorkerModel proves a resolved prompt's Config
// rebinds the round's model, and an invalid override falls back to the
// static binding instead of failing the round (#1421 P2 item 4).
func TestPromptBindingOverridesWorkerModel(t *testing.T) {
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

	bindingSrc := &stubBindingSource{}
	res := artifactsrc.New("stub", bindingSrc, time.Nanosecond)

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"stub-test":  {Kind: "openai", Endpoint: "http://fake-provider.invalid"},
			"stub-other": {Kind: "openai", Endpoint: "http://fake-provider.invalid"},
		},
		Models: map[string]config.ModelConfig{
			"any-model":   {Provider: "stub-test"},
			"bound-model": {Provider: "stub-other", Effort: "high"},
		},
		Agents: map[string]config.AgentConfig{
			"tester": {
				// Must be "agents/<x>" so artifactsrc.BundleName resolves a name -
				// bundledir falls back to the embedded copy when disk-in-cwd misses.
				Bundle:   "agents/web-researcher",
				Provider: "stub-test",
				Model:    "any-model",
			},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}

	var setupFn dag.SetupFunc
	artifacts := artifact.InMemoryService()
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, res, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifacts, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	na, ok := clientMap["tester"].(nativeAgent)
	if !ok {
		t.Fatalf("clientMap[%q] = %T, want nativeAgent", "tester", clientMap["tester"])
	}
	_, wm, _, _, refreshPrompt, release, err := na.ForNode("test-plan:test-node", nil, artifacts, "quack-test", "u1", "chat-1", "test-node", nil)
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release(false)

	// No override yet: the round runs on the static binding.
	bindingSrc.cfg = nil
	refreshPrompt(context.Background())
	if got := wm.Name(); got != "any-model" {
		t.Errorf("Name() = %q, want the static any-model with no override", got)
	}

	// A valid override rebinds the model (and its provider/effort with it).
	bindingSrc.cfg = map[string]any{"model": "bound-model"}
	refreshPrompt(context.Background())
	if got := wm.Name(); got != "bound-model" {
		t.Errorf("Name() = %q, want bound-model after a valid override", got)
	}

	// An invalid override falls back to the static binding rather than failing the round.
	bindingSrc.cfg = map[string]any{"model": "no-such-model"}
	refreshPrompt(context.Background())
	if got := wm.Name(); got != "any-model" {
		t.Errorf("Name() = %q, want the static any-model after an invalid override", got)
	}
}
