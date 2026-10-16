package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain doubles as the fake engine: the shim spawns this test binary with FAKE_ENGINE=1.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_ENGINE") == "1" {
		fakeEngine()
		return
	}
	os.Exit(m.Run())
}

func flagVal(name string) string {
	if i := slices.Index(os.Args, name); i >= 0 && i+1 < len(os.Args) {
		return os.Args[i+1]
	}
	return ""
}

// fakeEngine stands in for pig-quack: it reports the gate in like quackgate and replays one scripted turn per prompt.
func fakeEngine() {
	dir := os.Getenv("PIG_CODING_AGENT_DIR")
	_ = writeJSON(filepath.Join(dir, "launch.json"), map[string]any{"argv": os.Args[1:], "offline": os.Getenv("PI_OFFLINE")})
	var bridge struct {
		Prefix   string `json:"prefix"`
		PermPort int    `json:"permPort"`
		Tools    []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "extensions", "quackmcp.json"))
	_ = json.Unmarshal(b, &bridge)
	id, sessDir := flagVal("--session-id"), flagVal("--session-dir")
	_ = os.MkdirAll(sessDir, 0o755)
	_ = os.WriteFile(filepath.Join(sessDir, "fake_"+id+".jsonl"), nil, 0o644)
	if os.Getenv("FAKE_BLANK_RESUME") == "1" {
		fmt.Fprintf(os.Stderr, "Warning: No project session found with id '%s'\n", id)
	}
	if os.Getenv("FAKE_SILENT_GATE") != "1" {
		res, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/ready", bridge.PermPort), "application/json", nil)
		if err == nil {
			_ = res.Body.Close()
		}
	}
	out := json.NewEncoder(os.Stdout)
	emit := func(v map[string]any) { _ = out.Encode(v) }
	ask := func(path string) bool {
		body, _ := json.Marshal(map[string]any{"toolName": "read", "title": "read " + path, "input": map[string]any{"path": path}})
		res, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/perm", bridge.PermPort), "application/json", strings.NewReader(string(body)))
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		var r struct{ Allow bool }
		_ = json.NewDecoder(res.Body).Decode(&r)
		return r.Allow
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var cmd struct{ Type, Message string }
		_ = json.Unmarshal(sc.Bytes(), &cmd)
		fakeCommand(cmd.Type, cmd.Message, id, sessDir, bridge.Prefix, len(bridge.Tools) > 0, emit, ask)
	}
}

func fakeCommand(typ, msg, id, sessDir, prefix string, bridged bool, emit func(map[string]any), ask func(string) bool) {
	switch {
	case typ == "steer":
		emit(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "[steered: " + msg + "]"}})
	case typ == "abort":
		emit(map[string]any{"type": "agent_settled"})
	case typ != "prompt":
	case msg == "settle-silently":
		emit(map[string]any{"type": "agent_settled"})
	case msg == "STALL":
		// Settles only when the abort arrives.
	case msg == "EXIT":
		os.Exit(3)
	default:
		f, _ := os.OpenFile(filepath.Join(sessDir, id+".turns"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		_, _ = f.WriteString("1\n")
		_ = f.Close()
		fakeTurn(prefix, bridged, emit, ask)
	}
}

func fakeTurn(prefix string, bridged bool, emit func(map[string]any), ask func(string) bool) {
	type m = map[string]any
	emit(m{"type": "message_update", "assistantMessageEvent": m{"type": "thinking_delta", "delta": "hmm"}})
	emit(m{"type": "message_end", "message": m{"role": "assistant", "stopReason": "toolUse", "usage": m{"input": 5, "output": 1, "cacheRead": 2, "totalTokens": 42}}})
	emit(m{"type": "tool_execution_start", "toolCallId": "call_1", "toolName": "bash", "args": m{"command": "echo hi"}})
	emit(m{"type": "tool_execution_end", "toolCallId": "call_1", "result": m{"content": []m{{"type": "text", "text": "hi\n"}}}})
	if bridged {
		emit(m{"type": "tool_execution_start", "toolCallId": "call_mcp", "toolName": prefix + "_stage_review", "args": m{"verdict": "approve"}})
		emit(m{"type": "tool_execution_end", "toolCallId": "call_mcp", "result": m{"content": []m{{"type": "text", "text": "staged"}}}})
	}
	for i, path := range []string{"app/.env", "app/.env.prod"} {
		id := fmt.Sprintf("call_g%d", i+1)
		emit(m{"type": "tool_execution_start", "toolCallId": id, "toolName": "read", "args": m{"path": path}})
		emit(m{"type": "tool_execution_end", "toolCallId": id, "isError": !ask(path)})
	}
	emit(m{"type": "message_update", "assistantMessageEvent": m{"type": "text_delta", "delta": "done: hi"}})
	emit(m{"type": "message_end", "message": m{"role": "assistant", "stopReason": "stop", "usage": m{"input": 10, "output": 2, "cacheRead": 6, "cacheWrite": 3, "totalTokens": 99}}})
	emit(m{"type": "agent_settled"})
}

// testClient is quack's side of the connection: it records updates and answers permission asks, allow first.
type testClient struct {
	mu      sync.Mutex
	updates []sdk.SessionUpdate
	asks    []sdk.RequestPermissionRequest
}

func (c *testClient) SessionUpdate(_ context.Context, n sdk.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, n.Update)
	return nil
}

func (c *testClient) RequestPermission(_ context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks = append(c.asks, p)
	opt := sdk.PermissionOptionId("reject")
	if len(c.asks) == 1 {
		opt = "allow"
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeSelected(opt)}, nil
}

func (c *testClient) snapshot() []sdk.SessionUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.updates)
}

func (*testClient) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound("fs/read_text_file")
}

func (*testClient) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound("fs/write_text_file")
}

func (*testClient) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound("terminal/create")
}

func (*testClient) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound("terminal/kill")
}

func (*testClient) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound("terminal/output")
}

func (*testClient) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound("terminal/release")
}

func (*testClient) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound("terminal/wait_for_exit")
}

func (*testClient) UnstableCompleteElicitation(context.Context, sdk.UnstableCompleteElicitationNotification) error {
	return nil
}

// connect runs a shim in-process on pipes, with this test binary as its engine.
func connect(t *testing.T, cfg config, stateRoot string) (*sdk.ClientSideConnection, *testClient, *shim) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := newShim(cfg, stateRoot, exe)
	agentIn, clientOut := io.Pipe()
	clientIn, agentOut := io.Pipe()
	s.setConn(sdk.NewAgentSideConnection(s, agentOut, agentIn))
	c := &testClient{}
	conn := sdk.NewClientSideConnection(c, clientOut, clientIn)
	t.Cleanup(func() {
		s.stopEngine()
		_ = clientOut.Close()
		_ = agentOut.Close()
	})
	if _, err := conn.Initialize(context.Background(), sdk.InitializeRequest{ProtocolVersion: sdk.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	return conn, c, s
}

// mcpURL serves a real MCP server with two review tools, standing in for quack's loopback server.
func mcpURL(t *testing.T) string {
	srv := mcp.NewServer(&mcp.Implementation{Name: "quackmcp", Version: "0"}, nil)
	for _, name := range []string{"stage_review", "list_review_comments"} {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	}
	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(hs.Close)
	return hs.URL
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func prompt(t *testing.T, conn *sdk.ClientSideConnection, id sdk.SessionId, text string) (sdk.PromptResponse, error) {
	t.Helper()
	return conn.Prompt(context.Background(), sdk.PromptRequest{SessionId: id, Prompt: []sdk.ContentBlock{sdk.TextBlock(text)}})
}

func TestSessionWritesEngineConfig(t *testing.T) {
	t.Setenv("FAKE_ENGINE", "1")
	root := t.TempDir()
	cfg := config{Endpoint: "http://127.0.0.1:1/v1", Model: "stub", ContextWindow: 65536, MaxOutputTokens: 32768,
		SkillPaths: []string{"/opt/quack/skills"}, OpenRouterRouting: map[string]any{"order": []any{"Together"}}}
	conn, _, s := connect(t, cfg, root)
	sess, err := conn.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: root, McpServers: []sdk.McpServer{{Http: &sdk.McpServerHttpInline{Name: "quackmcp", Url: mcpURL(t), Type: "http"}}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := s.sessionDir(sess.SessionId)
	var models struct {
		Providers map[string]struct {
			APIKey string         `json:"apiKey"`
			Compat map[string]any `json:"compat"`
			Models []map[string]any
		}
	}
	readJSON(t, filepath.Join(dir, "models.json"), &models)
	q := models.Providers["quack"]
	if q.Models[0]["contextWindow"] != 65536.0 || q.Models[0]["maxTokens"] != 32768.0 || q.APIKey != "unused" || q.Compat["openRouterRouting"] == nil {
		t.Errorf("models.json %+v", q)
	}
	var settings struct{ Skills []string }
	readJSON(t, filepath.Join(dir, "settings.json"), &settings)
	if !slices.Equal(settings.Skills, cfg.SkillPaths) {
		t.Errorf("skills %v", settings.Skills)
	}
	var bridge struct {
		Prefix     string
		AllowClone bool
		Tools      []struct{ Name string }
	}
	readJSON(t, filepath.Join(dir, "extensions", "quackmcp.json"), &bridge)
	if bridge.Prefix != "quackmcp" || bridge.AllowClone || len(bridge.Tools) != 2 {
		t.Errorf("quackmcp.json %+v", bridge)
	}
	var launch struct {
		Argv    []string
		Offline string
	}
	readJSON(t, filepath.Join(dir, "launch.json"), &launch)
	want := "read,bash,edit,write,quackmcp_list_review_comments,quackmcp_stage_review"
	if i := slices.Index(launch.Argv, "--tools"); i < 0 || launch.Argv[i+1] != want || !slices.Contains(launch.Argv, "--no-extensions") || launch.Offline != "1" {
		t.Errorf("launch %+v", launch)
	}
}

func TestPromptRelaysTheTurn(t *testing.T) {
	t.Setenv("FAKE_ENGINE", "1")
	root := t.TempDir()
	conn, c, _ := connect(t, config{Model: "stub"}, root)
	sess, err := conn.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: root, McpServers: []sdk.McpServer{{Sse: &sdk.McpServerSseInline{Name: "quackmcp", Url: mcpURL(t), Type: "sse"}}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := prompt(t, conn, sess.SessionId, "do the thing")
	if err != nil || resp.StopReason != sdk.StopReasonEndTurn || resp.Meta["pi_stop_reason"] != "stop" {
		t.Fatalf("prompt %+v err %v", resp, err)
	}
	var thought, chunk bool
	var usages []*sdk.SessionUsageUpdate
	calls := map[string]*sdk.SessionUpdateToolCall{}
	ends := map[string]*sdk.SessionToolCallUpdate{}
	for _, u := range c.snapshot() {
		thought = thought || u.AgentThoughtChunk != nil
		chunk = chunk || u.AgentMessageChunk != nil
		if u.UsageUpdate != nil {
			usages = append(usages, u.UsageUpdate)
		}
		if u.ToolCall != nil {
			calls[string(u.ToolCall.ToolCallId)] = u.ToolCall
		}
		if u.ToolCallUpdate != nil {
			ends[string(u.ToolCallUpdate.ToolCallId)] = u.ToolCallUpdate
		}
	}
	if !thought || !chunk {
		t.Errorf("thought %v chunk %v", thought, chunk)
	}
	if b := calls["call_1"]; b.Kind != sdk.ToolKindExecute || ends["call_1"].RawOutput.(map[string]any)["output"] != "hi\n" {
		t.Errorf("bash call %+v end %+v", b, ends["call_1"])
	}
	if m := calls["call_mcp"]; m.Kind != sdk.ToolKindEdit || m.Meta["quack_mcp_tool"] != "stage_review" {
		t.Errorf("bridged call %+v", m)
	}
	if len(c.asks) != 2 || *ends["call_g1"].Status != sdk.ToolCallStatusCompleted || *ends["call_g2"].Status != sdk.ToolCallStatusFailed {
		t.Errorf("asks %d, g1 %v, g2 %v", len(c.asks), *ends["call_g1"].Status, *ends["call_g2"].Status)
	}
	// input(10) + cacheRead(6) + cacheWrite(3): prompt tokens are the full total, cached a subset.
	if len(usages) != 2 || usages[1].Used != 99 || usages[1].Meta["quack_prompt_tokens"] != 19.0 || usages[1].Meta["quack_cached_tokens"] != 6.0 || usages[1].Meta["quack_completion_tokens"] != 2.0 {
		t.Errorf("usage updates %+v", usages)
	}
	if resp, _ := prompt(t, conn, sess.SessionId, "settle-silently"); resp.Meta["pi_stop_reason"] != "none" {
		t.Errorf("silent settle meta %v", resp.Meta)
	}
}

func TestSteerAndCancel(t *testing.T) {
	t.Setenv("FAKE_ENGINE", "1")
	root := t.TempDir()
	conn, c, _ := connect(t, config{}, root)
	sess, err := conn.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: root, McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.CallExtension(context.Background(), "_quack/steer", map[string]string{"text": "early"}); err == nil {
		t.Error("steer with no live round accepted")
	}
	done := make(chan sdk.PromptResponse)
	go func() {
		resp, _ := prompt(t, conn, sess.SessionId, "STALL")
		done <- resp
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := conn.CallExtension(context.Background(), "_quack/steer", map[string]string{"text": "focus"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("steer never reached the live round")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := conn.Cancel(context.Background(), sdk.CancelNotification{SessionId: sess.SessionId}); err != nil {
		t.Fatal(err)
	}
	if resp := <-done; resp.StopReason != sdk.StopReasonCancelled {
		t.Errorf("cancelled prompt stop %v", resp.StopReason)
	}
	steered := slices.ContainsFunc(c.snapshot(), func(u sdk.SessionUpdate) bool {
		return u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text.Text == "[steered: focus]"
	})
	if !steered {
		t.Error("steer text never reached the engine")
	}
	if resp, err := prompt(t, conn, sess.SessionId, "again"); err != nil || resp.StopReason != sdk.StopReasonEndTurn {
		t.Errorf("session wedged after cancel: %v %v", resp, err)
	}
	if _, err := prompt(t, conn, sess.SessionId, "EXIT"); err == nil {
		t.Error("engine exit mid-prompt not reported")
	}
}

func TestLoadSessionResumes(t *testing.T) {
	t.Setenv("FAKE_ENGINE", "1")
	root := t.TempDir()
	conn, _, s := connect(t, config{AllowClone: true}, root)
	if _, err := conn.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: "pi-gone", Cwd: root, McpServers: []sdk.McpServer{}}); err == nil || !strings.Contains(err.Error(), "no persisted session") {
		t.Errorf("load of an unknown session: %v", err)
	}
	sess, err := conn.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: root, McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prompt(t, conn, sess.SessionId, "one"); err != nil {
		t.Fatal(err)
	}
	var bridge struct{ AllowClone bool }
	readJSON(t, filepath.Join(s.sessionDir(sess.SessionId), "extensions", "quackmcp.json"), &bridge)
	if !bridge.AllowClone {
		t.Error("allow_clone never reached quackgate's config")
	}

	conn2, _, _ := connect(t, config{}, root)
	if _, err := conn2.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: sess.SessionId, Cwd: root, McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := prompt(t, conn2, sess.SessionId, "two"); err != nil {
		t.Fatal(err)
	}
	turns, _ := os.ReadFile(filepath.Join(s.sessionDir(sess.SessionId), "pi-sessions", string(sess.SessionId)+".turns"))
	if strings.Count(string(turns), "\n") != 2 {
		t.Errorf("load spawned the engine against another session dir: turns %q", turns)
	}

	t.Setenv("FAKE_BLANK_RESUME", "1")
	conn3, _, _ := connect(t, config{}, root)
	if _, err := conn3.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: sess.SessionId, Cwd: root, McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := prompt(t, conn3, sess.SessionId, "three"); err == nil || !strings.Contains(err.Error(), "did not actually resume") {
		t.Errorf("blank resume not refused: %v", err)
	}
}

func TestUngatedEngineGetsNoSession(t *testing.T) {
	t.Setenv("FAKE_ENGINE", "1")
	t.Setenv("FAKE_SILENT_GATE", "1")
	prev := gateReadyTimeout
	gateReadyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { gateReadyTimeout = prev })
	root := t.TempDir()
	conn, _, _ := connect(t, config{}, root)
	if _, err := conn.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: root, McpServers: []sdk.McpServer{}}); err == nil || !strings.Contains(err.Error(), "quackgate did not report in") {
		t.Errorf("ungated engine got a session: %v", err)
	}
	if _, err := prompt(t, conn, "x", "hi"); err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Errorf("prompt without a session: %v", err)
	}
}

func TestMCPKind(t *testing.T) {
	for name, want := range map[string]sdk.ToolKind{"read_artifact": sdk.ToolKindRead, "recall_memory": sdk.ToolKindRead,
		"unstage_review_comment": sdk.ToolKindDelete, "stage_review": sdk.ToolKindEdit, "frobnicate": sdk.ToolKindEdit} {
		if got := mcpKind(name); got != want {
			t.Errorf("mcpKind(%s) = %s, want %s", name, got, want)
		}
	}
}
