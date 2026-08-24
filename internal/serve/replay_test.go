package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/workspace"
)

// runnableTool is the structural interface a functiontool-built tool.Tool
// satisfies beyond the plain tool.Tool interface (Name/Description/
// IsLongRunning) - mirrors internal/tools' own (unexported) runnableTool.
// Redeclared here because this is a different package and Go interface
// satisfaction is structural, not nominal.
type runnableTool interface {
	Name() string
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// writeCurrentDateReplayFixture writes a one-entry ledger JSONL recording a
// current_date call under the ZERO-VALUE ledger coordinates (no node/agent/
// round attributes) - what a bare ctx-less Run(nil, ...) call resolves to
// (ledger.CoordsFromContext(nil) == ledger.Coords{}), so the test can invoke
// the built stub directly without assembling a real agent.Context. The
// canned result is a value no REAL current_date implementation could ever
// produce, so a passing assertion proves the STUB answered, not a live call.
func writeCurrentDateReplayFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	line := `{"timestamp":"2026-01-01T00:00:00Z","attributes":{` +
		`"gen_ai.operation.name":"execute_tool","gen_ai.tool.name":"current_date",` +
		`"gen_ai.tool.call.result":"{\"result\":\"RECORDED-NOT-LIVE\"}"` +
		`}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBuildAgents_ReplayProvider_NativeAgentToolsAreStubs is #610's
// regression test: a native (non-ACP) agent bound to a kind:"replay"
// provider must get REPLAY STUBS for its tools (Deps.Replayer set), not real
// backends - a full-server strict replay (`quack replay`) of a chat that
// used native agents must never construct, let alone execute, a live tool.
// Before #610, buildAgents never threaded Deps.Replayer at all: this agent's
// current_date tool would have been the REAL implementation, which reports
// today's actual date - never the fixture's canned "RECORDED-NOT-LIVE".
func TestBuildAgents_ReplayProvider_NativeAgentToolsAreStubs(t *testing.T) {
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
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, nil, jail, nil, nil, nil, nil, nil, nil, nil, &setupFn, nil)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	na, ok := clientMap["tester"].(nativeAgent)
	if !ok {
		t.Fatalf("clientMap[%q] = %T, want nativeAgent", "tester", clientMap["tester"])
	}
	_, _, tools, release, err := na.ForNode("test-plan:test-node", nil)
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release()

	if len(tools) != 1 {
		t.Fatalf("built %d tools, want 1 (current_date)", len(tools))
	}
	rt, ok := tools[0].(runnableTool)
	if !ok {
		t.Fatalf("tools[0] = %T, does not implement Run - can't invoke it to check stub vs live", tools[0])
	}
	if got := rt.Name(); got != "current_date" {
		t.Fatalf("tools[0].Name() = %q, want current_date", got)
	}
	res, err := rt.Run(nil, map[string]any{})
	if err != nil {
		t.Fatalf("Run: %v - a real current_date tool never errors; this smells like a live backend, not a stub", err)
	}
	if got := res["result"]; got != "RECORDED-NOT-LIVE" {
		t.Fatalf("current_date result = %v, want the fixture's canned \"RECORDED-NOT-LIVE\" - "+
			"a real value here means a LIVE tool was built despite the replay provider (#610)", got)
	}
}
