package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestMemoryMCPServers_SSEWireShape pins the session/new wire shape opencode
// requires: an SSE server with type "sse" and a non-null headers array. The
// original Http variant (type unset, headers nil) serialized to
// {"type":"","headers":null,...}, which opencode rejected with -32602 and
// killed the ACP subprocess - breaking every code node. Guard against regress.
func TestMemoryMCPServers_SSEWireShape(t *testing.T) {
	caps := sdk.AgentCapabilities{McpCapabilities: sdk.McpCapabilities{Http: true}}

	// No secret ⇒ empty (but non-nil) slice.
	if got := memoryMCPServers("", caps); got == nil || len(got) != 0 {
		t.Fatalf("no secret: want empty non-nil slice, got %#v", got)
	}
	// No MCP capability ⇒ empty.
	if got := memoryMCPServers("abc", sdk.AgentCapabilities{}); len(got) != 0 {
		t.Fatalf("no mcp capability: want empty, got %#v", got)
	}

	servers := memoryMCPServers("deadbeef", caps)
	if len(servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(servers))
	}
	s := servers[0]
	if s.Sse == nil || s.Http != nil {
		t.Fatalf("want the SSE variant (not Http), got %#v", s)
	}
	if s.Sse.Type != "sse" {
		t.Errorf("Type = %q, want \"sse\"", s.Sse.Type)
	}
	if s.Sse.Headers == nil {
		t.Error("Headers is nil; opencode needs a non-null array")
	}
	// And the marshaled JSON must carry type:"sse" and headers:[] (not null).
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	if !strings.Contains(js, `"type":"sse"`) || strings.Contains(js, `"headers":null`) {
		t.Errorf("wire JSON must have type:\"sse\" and a non-null headers array; got %s", js)
	}
}

// fakeMCPEmbedder returns a fixed unit vector for every text, so a recall
// query matches any stored point (cosine = 1) - the round-trip below is
// exercised through the SCOPE filter, not embedding similarity.
type fakeMCPEmbedder struct{}

func (fakeMCPEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// verbatimConsolidator replies with a single ADD op that writes sourceText
// verbatim - just enough to seed a bucket through the real Store.Commit path.
type verbatimConsolidator struct{}

func (verbatimConsolidator) Name() string { return "verbatim-consolidator" }

func (verbatimConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var text string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					text = p.Text
				}
			}
		}
		start := strings.Index(text, "FINAL ANSWER (extract additional durable tradecraft from it):\n")
		content := "seed"
		if start >= 0 {
			rest := text[start+len("FINAL ANSWER (extract additional durable tradecraft from it):\n"):]
			if end := strings.IndexByte(rest, '\n'); end >= 0 {
				content = rest[:end]
			}
		}
		reply := `{"ops":[{"action":"ADD","content":"` + content + `","kind":"note"}]}`
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

// mustMemSecret mints a fresh unguessable secret, failing the test on error.
func mustMemSecret(t *testing.T) string {
	t.Helper()
	secret, err := vetting.NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	return secret
}

// connectMCP dials the memory MCP handler at path (a secret, a guessed
// advisor-thread token, or any other string a caller wants to try).
func connectMCP(t *testing.T, ts *httptest.Server, path string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/" + path}, nil)
	if err != nil {
		t.Fatalf("connect %q: %v", path, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestMemoryMCP_LoadMemory_ScopedRecall proves load_memory resolves ONLY the
// buckets the registered node is entitled to - never a bucket named by the tool
// call itself, satisfying #344's "scope from registration, not args" rule.
func TestMemoryMCP_LoadMemory_ScopedRecall(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMCPEmbedder{}, verbatimConsolidator{}, "test_mcp_load", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	inScope := memory.Scope{Repo: "acme/in-scope"}
	outOfScope := memory.Scope{Repo: "acme/out-of-scope"}
	if _, err := store.Commit(ctx, inScope, "seed", memory.Provenance{}, nil, "run go test ./... before every commit"); err != nil {
		t.Fatalf("seed in-scope: %v", err)
	}
	if _, err := store.Commit(ctx, outOfScope, "seed", memory.Provenance{}, nil, "the other repo uses npm test"); err != nil {
		t.Fatalf("seed out-of-scope: %v", err)
	}

	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Memory: store, Scope: inScope})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "load_memory", Arguments: map[string]any{"query": "test commands"}})
	if err != nil {
		t.Fatalf("CallTool load_memory: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "go test") {
		t.Fatalf("load_memory result missing the in-scope memory: %q", text)
	}
	if strings.Contains(text, "npm test") {
		t.Fatalf("load_memory leaked an out-of-scope memory: %q", text)
	}
}

// TestMemoryMCP_StageMemory_LandsInBuffer proves stage_memory writes into the
// NODE's own MemStage - the buffer the gate drains into commitMemoryOnPass -
// resolved from the URL's secret, never a tool argument.
func TestMemoryMCP_StageMemory_LandsInBuffer(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMCPEmbedder{}, verbatimConsolidator{}, "test_mcp_stage", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	secret := mustMemSecret(t)
	stage := &vetting.MemStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Memory: store, Staged: stage})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_memory",
		Arguments: map[string]any{"content": "the build runs via make build", "kind": "repo"},
	})
	if err != nil {
		t.Fatalf("CallTool stage_memory: %v", err)
	}
	if res.IsError {
		t.Fatalf("stage_memory returned an error: %s", toolResultText(t, res))
	}

	staged := stage.Drain()
	if len(staged) != 1 || staged[0].Content != "the build runs via make build" {
		t.Fatalf("staged = %+v, want exactly one candidate with the tool's content", staged)
	}
	if staged[0].Metadata["bucket"] != "repo" {
		t.Fatalf("staged bucket = %q, want %q", staged[0].Metadata["bucket"], "repo")
	}
}

// TestMemoryMCP_CrossNodeIsolation is the negative test for #344's security
// fix: two concurrent nodes of the SAME plan get two DISTINCT, unguessable
// secrets. Node A's client must not be able to read or write node B's memory
// - not via B's real secret misused by A's assumptions, and NOT by deriving
// anything from the plan/node IDs both nodes' prompts disclose (the advisor-
// thread token, planID+"/"+nodeID, is exactly that derivation, and a sibling's
// node ID is visible in a worker's own prompt via the running-siblings list).
func TestMemoryMCP_CrossNodeIsolation(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMCPEmbedder{}, verbatimConsolidator{}, "test_mcp_isolation", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	scopeA := memory.Scope{Repo: "acme/node-a-repo"}
	scopeB := memory.Scope{Repo: "acme/node-b-repo"}
	if _, err := store.Commit(ctx, scopeA, "seed", memory.Provenance{}, nil, "node A's secret build note"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := store.Commit(ctx, scopeB, "seed", memory.Provenance{}, nil, "node B's secret build note"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	secretA, secretB := mustMemSecret(t), mustMemSecret(t)
	stageA, stageB := &vetting.MemStage{}, &vetting.MemStage{}
	vetting.RegisterMemSession(secretA, vetting.MemSession{Memory: store, Scope: scopeA, Staged: stageA})
	defer vetting.UnregisterMemSession(secretA)
	vetting.RegisterMemSession(secretB, vetting.MemSession{Memory: store, Scope: scopeB, Staged: stageB})
	defer vetting.UnregisterMemSession(secretB)

	// Same plan, two sibling nodes - the OLD (vulnerable) credential would have
	// been these two advisor-thread tokens, each derivable from the other by
	// any agent that knows its own node ID and its plan ID (both disclosed in
	// its own prompt) plus a sibling's node ID (disclosed via the running-
	// siblings list - see internal/dag/executor.go siblingIDs).
	planID := "plan-shared"
	tokenA := vetting.AdvisorThreadToken(planID, "node-a")
	tokenB := vetting.AdvisorThreadToken(planID, "node-b")
	vetting.RegisterAdvisorThread(tokenA, vetting.AdvisorTask{NodeID: "node-a", MemSecret: secretA})
	defer vetting.UnregisterAdvisorThread(tokenA)
	vetting.RegisterAdvisorThread(tokenB, vetting.AdvisorTask{NodeID: "node-b", MemSecret: secretB})
	defer vetting.UnregisterAdvisorThread(tokenB)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })

	// 1) Node A's client, using its OWN real secret, must see only its own bucket.
	csA := connectMCP(t, ts, secretA)
	res, err := csA.CallTool(ctx, &mcp.CallToolParams{Name: "load_memory", Arguments: map[string]any{"query": "build note"}})
	if err != nil {
		t.Fatalf("CallTool load_memory (A, own secret): %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "node A's secret") {
		t.Fatalf("A's own recall missing A's memory: %q", text)
	}
	if strings.Contains(text, "node B's secret") {
		t.Fatalf("A's own secret leaked B's memory: %q", text)
	}

	// 2) An attacker who only knows the DERIVABLE advisor-thread tokens (never
	// the real secrets) gets NOTHING - the tool isn't even registered for that
	// path, so the call fails loudly with a protocol-level error.
	for _, guess := range []string{tokenA, tokenB, planID + "/node-b", "node-b", ""} {
		csGuess := connectMCP(t, ts, guess)
		if _, err := csGuess.CallTool(ctx, &mcp.CallToolParams{Name: "load_memory", Arguments: map[string]any{"query": "build note"}}); err == nil {
			t.Fatalf("guessed path %q must NOT expose load_memory, but the call succeeded", guess)
		}
	}

	// 3) Node A's real secret cannot stage into node B's buffer: A can only
	// ever reach ITS OWN MemSession (the handler resolves tools from the ONE
	// session matching the URL's secret), so a stage_memory call over csA can
	// only ever land in stageA.
	if _, err := csA.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_memory",
		Arguments: map[string]any{"content": "poisoned by node A", "kind": "repo"},
	}); err != nil {
		t.Fatalf("CallTool stage_memory (A, own secret): %v", err)
	}
	if got := stageB.Drain(); len(got) != 0 {
		t.Fatalf("node B's staging buffer was reachable from node A's session: %+v", got)
	}
	if got := stageA.Drain(); len(got) != 1 || got[0].Content != "poisoned by node A" {
		t.Fatalf("node A's own stage_memory call didn't land in A's own buffer: %+v", got)
	}
}

// TestMemoryMCP_UnregisteredSecret_FailsLoudly pins the lifecycle guarantee: a
// straggler call after a node's session has been unregistered (the gate
// drains-and-unregisters the moment it reads the staging buffer - see
// RunGatedRefine) gets an explicit protocol error, never a silent write into
// an orphaned buffer nobody will read again.
func TestMemoryMCP_UnregisteredSecret_FailsLoudly(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMCPEmbedder{}, verbatimConsolidator{}, "test_mcp_gone", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	secret := mustMemSecret(t)
	stage := &vetting.MemStage{}
	vetting.RegisterMemSession(secret, vetting.MemSession{Memory: store, Staged: stage})
	vetting.UnregisterMemSession(secret) // node already finished and drained

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "stage_memory", Arguments: map[string]any{"content": "too late"}}); err == nil {
		t.Fatal("a straggler call against an unregistered secret must fail, not silently succeed")
	}
	if got := stage.Drain(); len(got) != 0 {
		t.Fatalf("straggler call must never reach the orphaned buffer, got %+v", got)
	}
}

// TestMemoryMCP_ConnectMarksSessionConnected pins #640's observability fix
// end to end: a real client connecting through memoryMCPHandler must mark the
// session connected (vetting.MarkMemSessionConnected), so UnregisterMemSession
// stays silent on teardown - the same silent "offered but unreachable" gap
// that let the #628 rename go unnoticed for a full day now warns instead (see
// vetting.TestUnregisterMemSession_WarnsWhenNeverConnected for the negative
// case, which can't be driven here since the handler always connects).
func TestMemoryMCP_ConnectMarksSessionConnected(t *testing.T) {
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: &vetting.ReviewStage{}})

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	connectMCP(t, ts, secret)

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	vetting.UnregisterMemSession(secret)
	slog.SetDefault(restore)

	if strings.Contains(buf.String(), "never connected") {
		t.Errorf("a real MCP connection should have marked the session connected; got warning: %s", buf.String())
	}
}

// TestMemoryMCPURL_LoopbackOnly confirms the server binds 127.0.0.1, never a
// wildcard address that would make it reachable off-host.
func TestMemoryMCPURL_LoopbackOnly(t *testing.T) {
	base := memoryMCPURL()
	if base == "" {
		t.Skip("memory MCP server unavailable in this environment")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	if h := u.Hostname(); h != "127.0.0.1" {
		t.Fatalf("memory MCP server host = %q, want 127.0.0.1 (loopback only)", h)
	}
}

// TestMemoryMCP_NamespaceIsSurfaceNeutral pins the shared per-node server's
// name: "quackmcp" - surface-neutral (not "quack-memory", since it also
// serves review/PR tools) and distinct from bare "quack" (opencode's own
// config names quack's LLM provider "quack" in the same config, so a
// collision there suppresses the tool prefix entirely). Checks both the
// server's own identity (initialize handshake) and the Name handed to
// opencode in session/new (memoryMCPServers - the one that drives the tool
// prefix).
func TestMemoryMCP_NamespaceIsSurfaceNeutral(t *testing.T) {
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: &vetting.ReviewStage{}, PRStage: &vetting.PRStage{}})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	if got := cs.InitializeResult().ServerInfo.Name; got != "quackmcp" {
		t.Errorf("server identity = %q, want \"quackmcp\" (not memory-prefixed, not colliding with the \"quack\" LLM provider)", got)
	}

	caps := sdk.AgentCapabilities{McpCapabilities: sdk.McpCapabilities{Http: true}}
	servers := memoryMCPServers("deadbeef", caps)
	if len(servers) != 1 || servers[0].Sse == nil {
		t.Fatalf("expected one SSE server, got %#v", servers)
	}
	if got := servers[0].Sse.Name; got != "quackmcp" {
		t.Errorf("session/new server name = %q, want \"quackmcp\" - this is what opencode prefixes tool names with", got)
	}
}

// TestReviewMCP_ToolNamesUnprefixed pins the review + PR tool names the ACP
// agent prompts (agents/code-reviewer, agents/code-implementer) hardcode: the
// wire-level MCP tool name is always the bare "stage_review_comment" etc, and
// opencode adds the "quack_" prefix client-side, so these strings - the ones
// grepped for elsewhere - must never change without a matching prompt update.
func TestReviewMCP_ToolNamesUnprefixed(t *testing.T) {
	secret := mustMemSecret(t)
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: &vetting.ReviewStage{}, PRStage: &vetting.PRStage{}})
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, want := range []string{"stage_review_comment", "list_review_comments", "unstage_review_comment", "stage_review", "stage_pr"} {
		if !got[want] {
			t.Errorf("tool %q not registered; got %v", want, got)
		}
	}
}

// TestMcpToolNames_AnnouncesEveryRegisteredTool guards the announcement-gap
// class of bug (read_artifact was registered on the loopback server but
// missing from mcpToolNames, so the agent never learned it existed): for a
// fully-populated session, every tool the server actually registers must
// appear in mcpToolNames's output. Add a field to this session when adding
// the next MCP tool and this test enforces the announcement stays in sync.
func TestMcpToolNames_AnnouncesEveryRegisteredTool(t *testing.T) {
	secret := mustMemSecret(t)
	sess := vetting.MemSession{
		Memory:    &memory.Store{},
		Staged:    &vetting.MemStage{},
		Review:    &vetting.ReviewStage{},
		PRStage:   &vetting.PRStage{},
		Artifacts: artifactStub{},
		AppName:   "quack", UserID: "u1", ChatID: "chat-a",
	}
	vetting.RegisterMemSession(secret, sess)
	defer vetting.UnregisterMemSession(secret)

	ts := httptest.NewServer(memoryMCPHandler())
	t.Cleanup(func() { ts.Close() })
	cs := connectMCP(t, ts, secret)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	announced := map[string]bool{}
	for _, name := range mcpToolNames(sess, true) {
		announced[name] = true
	}
	for _, tl := range res.Tools {
		full := mcpServerName + "_" + tl.Name
		if !announced[full] {
			t.Errorf("tool %q is registered on the loopback server but mcpToolNames never announces it - the agent can't discover it", tl.Name)
		}
	}
}

// artifactStub is a no-op artifact.Service satisfying MemSession.Artifacts
// for tests that only need Artifacts != nil to enable read_artifact's registration.
type artifactStub struct{ artifact.Service }

func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
