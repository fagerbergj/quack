// artifacttools_native_test.go: #1123 regression coverage - a native
// (non-ACP) gated node's worker must actually be given the ADK-native
// artifact tools (list/read/edit/write_<kind>), not just check_mermaid/
// format-markdown, or its revise round has nothing to read/edit its prior
// revision with.
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

// TestBuildAgents_NativeNodeGetsArtifactTools: with an artifact.Service
// wired, a native gated node's ForNode-built tool list must include
// list_artifacts/read_artifact/edit_artifact/write_artifact plus at least
// one write_<kind> tool - the set previously only reached the ACP loopback
// MCP surface (#1091/#1108), leaving native nodes (synthesizer,
// web-researcher) with nothing to revise with (#1123).
func TestBuildAgents_NativeNodeGetsArtifactTools(t *testing.T) {
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

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"replay-test": {Kind: "replay", Bundle: writeCurrentDateReplayFixture(t)},
		},
		Agents: map[string]config.AgentConfig{
			"tester": {
				Bundle:   "../../agents/web-researcher",
				Provider: "replay-test",
				Model:    "any-model",
				Tools:    []string{"current_date"},
			},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}

	var setupFn dag.SetupFunc
	artifacts := artifact.InMemoryService()
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, artifacts)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	na, ok := clientMap["tester"].(nativeAgent)
	if !ok {
		t.Fatalf("clientMap[%q] = %T, want nativeAgent", "tester", clientMap["tester"])
	}
	_, _, tools, setRoundCoords, release, err := na.ForNode("test-plan:test-node", nil, artifacts, "quack-test", "u1", "chat-1", "test-node")
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release()

	if setRoundCoords == nil {
		t.Error("setRoundCoords is nil, want a callback the gate can restamp round/turn/head-sha through")
	}

	got := map[string]bool{}
	for _, tl := range tools {
		got[tl.Name()] = true
	}
	for _, want := range []string{"list_artifacts", "read_artifact", "edit_artifact", "write_artifact"} {
		if !got[want] {
			t.Errorf("native node's tool list = %v, missing %q", toolNames(tools), want)
		}
	}
	if !got["write_finding"] {
		t.Errorf("native node's tool list = %v, missing write_finding (a structured write_<kind> tool)", toolNames(tools))
	}
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, len(tools))
	for i, tl := range tools {
		out[i] = tl.Name()
	}
	return out
}
