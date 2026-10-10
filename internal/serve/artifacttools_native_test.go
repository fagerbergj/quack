// A native node always gets the artifact read tools, and the write tools only when its config or card asks.
package serve

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestBuildAgents_NativeNodeArtifactToolsFollowConfig: list/read for all; write_<kind> only
// where tools: names it; write/edit_artifact for a card naming an artifact kind.
func TestBuildAgents_NativeNodeArtifactToolsFollowConfig(t *testing.T) {
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

	agent := func(bundle string, tools ...string) config.AgentConfig {
		return config.AgentConfig{Bundle: bundle, Provider: "stub-test", Model: "any-model", Tools: tools}
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"stub-test": {Kind: "openai", Endpoint: "http://fake-provider.invalid"},
		},
		Agents: map[string]config.AgentConfig{
			"researcher":  agent("../../agents/web-researcher", "current_date"),
			"synthesizer": agent("../../agents/synthesizer", "current_date", "write_code_review", "write_finding"),
			"analyst":     agent("testdata/plugins/sleeper/agents/fixture-analyst", "current_date"),
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}

	var setupFn dag.SetupFunc
	artifacts := artifact.InMemoryService()
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, nil, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifacts, nil, nil, nil, nil, newPerNodeServers(), nil)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	cases := map[string]struct{ want, absent []string }{
		"researcher":  {want: []string{"list_artifacts", "read_artifact", "current_date"}, absent: []string{"edit_artifact", "write_artifact", "write_code_review", "write_finding"}},
		"synthesizer": {want: []string{"read_artifact", "write_code_review", "write_finding"}, absent: []string{"edit_artifact", "write_artifact"}},
		"analyst":     {want: []string{"read_artifact", "write_artifact", "edit_artifact"}, absent: []string{"write_code_review", "write_finding"}},
	}
	for name, c := range cases {
		na, ok := clientMap[name].(nativeAgent)
		if !ok {
			t.Fatalf("clientMap[%q] = %T, want nativeAgent", name, clientMap[name])
		}
		_, _, tools, setRoundCoords, refreshPrompt, release, err := na.ForNode(context.Background(), "test-plan:"+name, "test-plan/"+name, nil, artifacts, "quack-test", "u1", "chat-1", name, nil)
		if err != nil {
			t.Fatalf("%s: ForNode: %v", name, err)
		}
		release(false)
		if refreshPrompt == nil || setRoundCoords == nil {
			t.Errorf("%s: refreshPrompt/setRoundCoords nil, want the per-dispatch hooks the gate drives", name)
		}
		got := map[string]bool{}
		for _, tl := range tools {
			got[tl.Name()] = true
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s: tools = %v, missing %q", name, toolNames(tools), w)
			}
		}
		for _, a := range c.absent {
			if got[a] {
				t.Errorf("%s: tools = %v, must not carry %q (not in its config or card)", name, toolNames(tools), a)
			}
		}
	}
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, len(tools))
	for i, tl := range tools {
		out[i] = tl.Name()
	}
	return out
}
