package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// turnRecorder records the turn_id each artifact save carries (the store row's turn_id column).
type turnRecorder struct {
	artifact.Service
	mu    sync.Mutex
	turns map[string]string
}

func (s *turnRecorder) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, _, _ string, _ []byte, turnID string) (*artifact.SaveResponse, error) {
	s.mu.Lock()
	s.turns[req.FileName] = turnID
	s.mu.Unlock()
	return s.Save(ctx, req)
}

// toolCallProvider answers the worker's first chat completion with a call to
// name and every later one (after the tool result) with plain text.
func toolCallProvider(t *testing.T, name string, args map[string]any) *httptest.Server {
	return scriptedProvider(t, name, args, `"role":"tool"`, nil)
}

// scriptedProvider keeps calling name until a request body contains doneWhen, then answers
// plain text; record, when set, sees every request body.
func scriptedProvider(t *testing.T, name string, args map[string]any, doneWhen string, record func(body string)) *httptest.Server {
	t.Helper()
	argJSON, _ := json.Marshal(args)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if record != nil {
			record(string(body))
		}
		msg := `{"role":"assistant","content":"done"}`
		finish := "stop"
		if !strings.Contains(string(body), doneWhen) {
			call, _ := json.Marshal(map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": string(argJSON)}})
			msg, finish = `{"role":"assistant","content":"","tool_calls":[`+string(call)+`]}`, "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"` + finish + `","message":` + msg + `}]}`))
	}))
}

// stubNodeOpts are buildStubNodeAgent's optional wiring: a memory store, the repeat-guard
// trip hook, and a skills scope (none means no load_skill tool at all).
type stubNodeOpts struct {
	taskStore *memory.Store
	tripped   func(chatID, nodeID, msg string) bool
	skills    []string
}

// buildStubNodeAgent builds the "tutor" native agent against a stub provider,
// offering toolNames (resolved against builtins and extTools), and returns its workspace jail.
func buildStubNodeAgent(t *testing.T, providerURL string, toolNames []string, extTools []extTool, artifacts artifact.Service, opts stubNodeOpts) (nativeAgent, *workspace.Jail) {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	builtinSkillSrc := newSkillSource(nil)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillsource.New(builtinSkillSrc, jail, localUserID)})
	if err != nil {
		t.Fatal(err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)})
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"stub": {Kind: "openai", Endpoint: providerURL}},
		Agents: map[string]config.AgentConfig{
			"tutor": {Bundle: "../../agents/web-researcher", Provider: "stub", Model: "m", Tools: toolNames, Skills: opts.skills},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none", MaxListEntries: 50},
	}
	var setupFn dag.SetupFunc
	clientMap, _, nodeServers, _, _, _, _, err := buildAgents(cfg, nil, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		opts.taskStore, jail, nil, extTools, nil, nil, opts.tripped, nil, nil, nil, nil, &setupFn, artifacts, nil, nil, nil, nil, newPerNodeServers(), nil)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	t.Cleanup(nodeServers.closeAll)
	return clientMap["tutor"].(nativeAgent), jail
}

// runStubNode drives one round of a node's worker (its A2A client) with msg as the user turn.
func runStubNode(t *testing.T, worker adkagent.Agent, msg string) {
	t.Helper()
	r, err := runner.New(runner.Config{AppName: "quack", Agent: worker, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	// A bare ctx, like the worker's own A2A request: no yield, no turn id.
	for _, err := range r.Run(context.Background(), "u1", "chat-1:n1", genai.NewContentFromText(msg, genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
}

// TestNativeNode_RenderUIEmitsAndStampsTurn: render_ui called by a real DAG
// node's worker (behind its A2A server, whose tool ctx carries neither the
// chat sink nor the turn id) still emits artifact_revision and stamps the chat turn.
func TestNativeNode_RenderUIEmitsAndStampsTurn(t *testing.T) {
	provider := toolCallProvider(t, "render_ui", map[string]any{"surface_id": "s1", "components": []any{map[string]any{"id": "root", "component": "Text", "text": "hi"}}})
	defer provider.Close()
	artifacts := &turnRecorder{Service: artifact.InMemoryService(), turns: map[string]string{}}
	agent, _ := buildStubNodeAgent(t, provider.URL, []string{"render_ui"}, nil, artifacts, stubNodeOpts{})

	var mu sync.Mutex
	var revs []stream.ArtifactRevisionData
	sink := func(ev stream.SSEEvent) {
		if d, ok := ev.Data.(stream.ArtifactRevisionData); ok {
			mu.Lock()
			revs = append(revs, d)
			mu.Unlock()
		}
	}
	ctx := stream.WithTurnID(context.Background(), "turn-7")
	worker, _, _, _, _, release, err := agent.ForNode(ctx, "p:n1", "p/n1", nil, artifacts, "quack", "u1", "chat-1", "n1", sink)
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release(false)

	runStubNode(t, worker, "render it")
	mu.Lock()
	defer mu.Unlock()
	if len(revs) != 1 || revs[0].ID != "a2ui_surface:s1" || revs[0].Kind != "a2ui_surface" || revs[0].NodeID != "n1" {
		t.Fatalf("artifact_revision events = %+v, want one for a2ui_surface:s1 from node n1", revs)
	}
	if got := artifacts.turns["a2ui_surface:s1"]; got != "turn-7" {
		t.Fatalf("surface turn_id = %q, want the chat turn turn-7", got)
	}
}
