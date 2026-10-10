package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain doubles as the fake ACP agent: tests re-exec this binary with QUACK_ACP_FAKE set,
// so each round runs against a real subprocess with no external dependency.
func TestMain(m *testing.M) {
	// Before the fake-agent intercept: the __reap wrapper inherits QUACK_ACP_FAKE too.
	workspace.RunSandboxExecIfInvoked()
	if mode := os.Getenv("QUACK_ACP_FAKE"); mode != "" {
		runFakeAgent(mode)
		os.Exit(0)
	}
	// Same wiring serve.go does: without it every test that pins a round would leak its subprocess,
	// since UnregisterAdvisorThread's cleanup is a no-op until this hook is set.
	vetting.NodeSessionClosed = ClosePinnedSession
	os.Exit(m.Run())
}

func runFakeAgent(mode string) {
	if mode == "deaf" {
		runDeafFakeAgent()
		return
	}
	ag := &fakeAgent{mode: mode}
	if mode == "steer" || mode == "idle-probe" {
		ag.steerCh = make(chan string, 1)
	}
	conn := sdk.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	ag.conn = conn
	<-conn.Done()
}

// runDeafFakeAgent answers initialize and session/new over raw JSON-RPC, then stops reading stdin;
// the SDK's connection always drains, so it can't simulate a child whose stdin pipe fills.
func runDeafFakeAgent() {
	r := bufio.NewReader(os.Stdin)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		_, _ = os.Stdout.Write(append(b, '\n'))
	}
	for range 2 { // initialize, session/new
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{"protocolVersion": sdk.ProtocolVersionNumber, "agentCapabilities": map[string]any{}})
		case "session/new":
			reply(req.ID, map[string]any{"sessionId": "s1"})
		default:
			return
		}
	}
	select {} // never read stdin again
}

type fakeAgent struct {
	mode string
	conn *sdk.AgentSideConnection
	// steerCh: mode "steer" blocks Prompt on this until steer text arrives.
	steerCh chan string
	// rounds counts Prompt calls this process served; only a pinned, reused process sees it exceed 1,
	// standing in for tool-call history carried across session/prompt calls.
	rounds int
}

// HandleExtensionMethod is the agent side of _quack/steer. Mode "steer-reject" mimics a shim
// that already settled the round: every call errors, like pi-acp.mjs's fail() path.
func (f *fakeAgent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != steerExtMethod {
		return map[string]any{}, nil
	}
	if f.mode == "steer-reject" {
		return nil, errors.New("no live round to steer")
	}
	if f.steerCh != nil {
		var p steerParams
		_ = json.Unmarshal(params, &p)
		f.steerCh <- p.Text
	}
	return map[string]any{}, nil
}

func (f *fakeAgent) Initialize(ctx context.Context, _ sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		// http:true mirrors real pi so a registered MemSecret exercises the mcpServers path; LoadSession only
		// for "resume*" modes, since an agent that never advertises it must never receive session/load.
		AgentCapabilities: sdk.AgentCapabilities{
			McpCapabilities: sdk.McpCapabilities{Http: true},
			LoadSession:     strings.HasPrefix(f.mode, "resume"),
		},
	}, nil
}

func (f *fakeAgent) NewSession(ctx context.Context, _ sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	return sdk.NewSessionResponse{SessionId: "s1"}, nil
}

// LoadSession: "resume" succeeds (the id comes back via the echoed prompt, there is no shared memory);
// "resume-fail"/"resume-then-fail" exercise the NewSession fallback and the post-resume error path.
func (f *fakeAgent) LoadSession(ctx context.Context, req sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	if f.mode == "resume-fail" {
		return sdk.LoadSessionResponse{}, errors.New("no such session")
	}
	return sdk.LoadSessionResponse{}, nil
}

func (f *fakeAgent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	send := func(u sdk.SessionUpdate) {
		_ = f.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: u})
	}
	switch f.mode {
	case "echo", "resume-echo":
		// Echoes exactly what crossed the subprocess boundary, so tests assert on what the harness sent.
		var text string
		if len(p.Prompt) > 0 && p.Prompt[0].Text != nil {
			text = p.Prompt[0].Text.Text
		}
		send(sdk.UpdateAgentMessageText(text))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	case "hang":
		// Cooperative: the SDK cancels this ctx on session/cancel.
		<-ctx.Done()
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	case "stubborn":
		// Ignores cancellation entirely - only the process-group kill ends it
		// (the v0.5.2 hang class).
		select {}
	case "usage":
		// Several streamed updates before the terminal response - proves the
		// metric seam fires once (on PromptResponse), not once per update.
		send(sdk.UpdateAgentThoughtText("planning"))
		send(sdk.StartToolCall("t1", "go test ./...", sdk.WithStartKind(sdk.ToolKindExecute)))
		send(sdk.UpdateToolCall("t1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))
		send(sdk.UpdateAgentMessageText("done"))
		cached, thoughts := 25, 10
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn, Usage: &sdk.Usage{
			InputTokens: 100, OutputTokens: 50, CachedReadTokens: &cached, ThoughtTokens: &thoughts,
		}}, nil
	case "usage-none":
		send(sdk.UpdateAgentMessageText("done"))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	case "steer":
		// Blocks until the extension delivers a forwarded message mid-round.
		text := <-f.steerCh
		send(sdk.UpdateAgentMessageText("steered: " + text))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	case "idle-probe":
		// Sends an update only when the test nudges via the steer extension, so activity timing is
		// under test control; blocks forever after, like "hang".
		for range 2 {
			<-f.steerCh
			send(sdk.UpdateAgentMessageText("ping"))
		}
		<-ctx.Done()
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	case "resume", "resume-fail":
		// Echoes the prompted session id: the only way the parent test sees it across the process boundary.
		send(sdk.UpdateAgentMessageText("session:" + string(p.SessionId)))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	case "resume-then-fail":
		return sdk.PromptResponse{}, errors.New("prompt boom")
	case "pin":
		f.rounds++
		send(sdk.UpdateAgentMessageText(fmt.Sprintf("round:%d session:%s", f.rounds, p.SessionId)))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	send(sdk.UpdateAgentThoughtText("planning"))
	send(sdk.StartToolCall("t1", "go test ./...",
		sdk.WithStartKind(sdk.ToolKindExecute),
		sdk.WithStartRawInput(map[string]any{"command": "go test ./..."})))
	send(sdk.UpdateToolCall("t1",
		sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted),
		sdk.WithUpdateRawOutput(map[string]any{"exit": 0, "output": "ok"})))
	send(sdk.UpdateAgentMessageText("did the "))
	send(sdk.UpdateAgentMessageText("thing"))
	return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
}

func (f *fakeAgent) Cancel(ctx context.Context, _ sdk.CancelNotification) error { return nil }
func (f *fakeAgent) Authenticate(ctx context.Context, _ sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}
func (f *fakeAgent) Logout(ctx context.Context, _ sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, nil
}
func (f *fakeAgent) CloseSession(ctx context.Context, _ sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, nil
}
func (f *fakeAgent) ListSessions(ctx context.Context, _ sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}
func (f *fakeAgent) ResumeSession(ctx context.Context, _ sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, nil
}
func (f *fakeAgent) SetSessionMode(ctx context.Context, _ sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, nil
}
func (f *fakeAgent) SetSessionConfigOption(ctx context.Context, _ sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, nil
}

func testAgent(t *testing.T, mode string) *Agent {
	t.Helper()
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=" + mode},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRound_FullPromptRound(t *testing.T) {
	a := testAgent(t, "happy")
	var specs []eventSpec
	err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("no events emitted")
	}
	final := specs[len(specs)-1]
	if final.partial || final.parts[0].Text != "did the thing" {
		t.Fatalf("final answer wrong: partial=%v %q", final.partial, final.parts[0].Text)
	}
	var sawThought, sawPair bool
	for _, s := range specs {
		for _, p := range s.parts {
			if p.Thought && p.Text == "planning" {
				sawThought = true
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "run_command" && !s.partial {
				sawPair = true
			}
		}
	}
	if !sawThought || !sawPair {
		t.Fatalf("stream incomplete: thought=%v durable run_command pair=%v", sawThought, sawPair)
	}
}

// TestRound_ResumesPriorSessionViaLoadSession: given a prior session id and an agent advertising LoadSession,
// the round resumes it via session/load and leaves the advisor thread's stored id unchanged.
func TestRound_ResumesPriorSessionViaLoadSession(t *testing.T) {
	a := testAgent(t, "resume")
	token := "tok-resume"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	var specs []eventSpec
	err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "continue", artifactsrc.Artifact{}, "", "", token, "prior-s1", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	got := specs[len(specs)-1].parts[0].Text
	if got != "session:prior-s1" {
		t.Fatalf("prompt targeted %q, want the resumed session prior-s1 (NewSession must not have been called)", got)
	}
	if task, _ := vetting.LookupAdvisorThread(token); task.ACPSessionID != "prior-s1" {
		t.Errorf("advisor thread session id = %q, want it to stay prior-s1 after a successful resume", task.ACPSessionID)
	}
}

// TestRound_LoadSessionFailureFallsBackToNewSession: a failing session/load must not fail the round;
// it falls back to session/new and the advisor thread picks up the fresh id.
func TestRound_LoadSessionFailureFallsBackToNewSession(t *testing.T) {
	a := testAgent(t, "resume-fail")
	token := "tok-resume-fail"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	var specs []eventSpec
	err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "continue", artifactsrc.Artifact{}, "", "", token, "prior-s1", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	got := specs[len(specs)-1].parts[0].Text
	if got != "session:s1" {
		t.Fatalf("prompt targeted %q, want the fallback NewSession id s1", got)
	}
	if task, _ := vetting.LookupAdvisorThread(token); task.ACPSessionID != "s1" {
		t.Errorf("advisor thread session id = %q, want the fresh NewSession id s1", task.ACPSessionID)
	}
}

// TestRound_PromptErrorAfterResumeClearsStoredSession: a resumed session that fails mid-round
// is cleared so the next round doesn't retry the same dead session.
func TestRound_PromptErrorAfterResumeClearsStoredSession(t *testing.T) {
	a := testAgent(t, "resume-then-fail")
	token := "tok-resume-then-fail"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ACPSessionID: "prior-s1"})
	defer vetting.UnregisterAdvisorThread(token)

	err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "continue", artifactsrc.Artifact{}, "", "", token, "prior-s1", func(eventSpec) bool { return true })
	if err == nil {
		t.Fatal("round: want an error from the fake agent's failing prompt")
	}
	if task, _ := vetting.LookupAdvisorThread(token); task.ACPSessionID != "" {
		t.Errorf("advisor thread session id = %q, want cleared after a resumed session's prompt failed", task.ACPSessionID)
	}
}

// TestRound_PinnedProcessReusedAcrossRounds: a second round for the same node reuses the live process
// and session (no session/new, no respawn) and carries state forward.
func TestRound_PinnedProcessReusedAcrossRounds(t *testing.T) {
	a := testAgent(t, "pin")
	token := "tok-pin"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	round := func() string {
		var specs []eventSpec
		if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", artifactsrc.Artifact{}, "", "", token, "", func(s eventSpec) bool {
			specs = append(specs, s)
			return true
		}); err != nil {
			t.Fatalf("round: %v", err)
		}
		return specs[len(specs)-1].parts[0].Text
	}

	first := round()
	if first != "round:1 session:s1" {
		t.Fatalf("round 1 = %q, want round:1 session:s1", first)
	}
	v, ok := pinned.Load(token)
	if !ok {
		t.Fatal("round 1 did not pin a process for the node")
	}
	pid1 := v.(*pinnedProc).h.cmd.Process.Pid

	second := round()
	if second != "round:2 session:s1" {
		t.Fatalf("round 2 = %q, want round:2 session:s1 (same process, same session, no session/new)", second)
	}
	v, ok = pinned.Load(token)
	if !ok {
		t.Fatal("round 2 evicted the pinned process instead of keeping it")
	}
	if pid2 := v.(*pinnedProc).h.cmd.Process.Pid; pid2 != pid1 {
		t.Fatalf("round 2 ran under pid %d, want the SAME pid %d as round 1", pid2, pid1)
	}
}

// TestRound_PreambleOnlyOnFreshSession: a pinned session's round 2 must not
// resend the preamble already in its own conversation.
func TestRound_PreambleOnlyOnFreshSession(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command:  []string{os.Args[0]},
		Env:      []string{"QUACK_ACP_FAKE=echo"},
		Home:     t.TempDir(),
		Jail:     jail,
		UserID:   "u1",
		Preamble: func(context.Context) string { return "PREAMBLE-TEXT" },
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "tok-preamble"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	round := func() string {
		var specs []eventSpec
		if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", artifactsrc.Artifact{}, "", "", token, "", func(s eventSpec) bool {
			specs = append(specs, s)
			return true
		}); err != nil {
			t.Fatalf("round: %v", err)
		}
		return specs[len(specs)-1].parts[0].Text
	}

	if first := round(); !strings.Contains(first, "PREAMBLE-TEXT") {
		t.Fatalf("round 1 (fresh session) must include the preamble, got: %q", first)
	}
	if second := round(); strings.Contains(second, "PREAMBLE-TEXT") {
		t.Fatalf("round 2 on the pinned session must not resend the preamble, got: %q", second)
	}
}

// TestRound_PreambleResentOnResumedSession: session/load runs on a fresh
// process, so it must still get the preamble - round 1's process is gone.
func TestRound_PreambleResentOnResumedSession(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command:  []string{os.Args[0]},
		Env:      []string{"QUACK_ACP_FAKE=resume-echo"},
		Home:     t.TempDir(),
		Jail:     jail,
		UserID:   "u1",
		Preamble: func(context.Context) string { return "PREAMBLE-TEXT" },
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "tok-preamble-resume"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	var specs []eventSpec
	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", artifactsrc.Artifact{}, "", "", token, "prior-s1", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	}); err != nil {
		t.Fatalf("round: %v", err)
	}
	got := specs[len(specs)-1].parts[0].Text
	if !strings.Contains(got, "PREAMBLE-TEXT") {
		t.Fatalf("a resumed session's fresh process must still get the preamble, got: %q", got)
	}
}

// TestClosePinnedSession_KillsProcessAndClearsRegistry: a node that finishes normally after pinning
// must not leave its process running (UnregisterAdvisorThread -> NodeSessionClosed -> ClosePinnedSession).
func TestClosePinnedSession_KillsProcessAndClearsRegistry(t *testing.T) {
	a := testAgent(t, "pin")
	token := "tok-close-pinned"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	var specs []eventSpec
	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", artifactsrc.Artifact{}, "", "", token, "", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	}); err != nil {
		t.Fatalf("round: %v", err)
	}
	v, ok := pinned.Load(token)
	if !ok {
		t.Fatal("round did not pin a process")
	}
	proc := v.(*pinnedProc).h.cmd.Process

	ClosePinnedSession(token) // simulates node-finish's own call, same as UnregisterAdvisorThread's hook

	if _, ok := pinned.Load(token); ok {
		t.Fatal("ClosePinnedSession left the process pinned")
	}
	// close() already waited on the killed process, so a signal probe reliably reports ESRCH without a timing window.
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("ClosePinnedSession did not kill the subprocess - it still responds to signals")
	}
}

// TestClosePinnedSession_KeepsACPStateDir: node finish keeps the session dir for a later session/load;
// only chat archive/delete (workspace.Jail.RemoveACPState) removes it.
func TestClosePinnedSession_KeepsACPStateDir(t *testing.T) {
	a := testAgent(t, "pin")
	token := "tok-close-pinned-state-dir"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	stateDir := filepath.Join(t.TempDir(), "acp-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{ACPStateDir: stateDir}, "go", artifactsrc.Artifact{}, "", "", token, "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	if _, ok := pinned.Load(token); !ok {
		t.Fatal("round did not pin a process")
	}

	ClosePinnedSession(token)

	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("ACPStateDir %q was removed by ClosePinnedSession: %v", stateDir, err)
	}
}

// TestUnregisterAdvisorThread_KillsPinnedProcess drives the boot wiring TestMain mirrors: graph.go's
// teardown only calls UnregisterAdvisorThread, so if it stops reaching the process every node leaks one.
func TestUnregisterAdvisorThread_KillsPinnedProcess(t *testing.T) {
	a := testAgent(t, "pin")
	token := "tok-unregister-kills-pin"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})

	if err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", artifactsrc.Artifact{}, "", "", token, "", func(eventSpec) bool { return true }); err != nil {
		t.Fatalf("round: %v", err)
	}
	if _, ok := pinned.Load(token); !ok {
		t.Fatal("round did not pin a process")
	}

	vetting.UnregisterAdvisorThread(token) // the ONLY call a finishing node makes (dag/graph.go)

	if _, ok := pinned.Load(token); ok {
		t.Fatal("node finish (UnregisterAdvisorThread) left the process pinned")
	}
}

// TestRound_AbortKillsPinnedProcess: CancelNode's abort during a round must still evict and kill
// the process; a killed process is never handed to the node's next round.
func TestRound_AbortKillsPinnedProcess(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var abort context.CancelFunc
	registered := make(chan struct{})
	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=hang"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterRoundAbort: func(chatID, nodeID string, cancel context.CancelFunc) {
			mu.Lock()
			abort = cancel
			mu.Unlock()
			close(registered)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "tok-abort-pin"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	done := make(chan error, 1)
	go func() {
		done <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "loop forever", artifactsrc.Artifact{}, "chat1", "n1", token, "", func(eventSpec) bool { return true })
	}()
	<-registered
	mu.Lock()
	cancel := abort
	mu.Unlock()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the aborted round")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("aborted round never returned")
	}
	if _, ok := pinned.Load(token); ok {
		t.Fatal("an aborted round must not leave its process pinned for the next round")
	}
}

// TestRound_FailedReuseFallsBackToFreshProcess: once a pinned process dies, the next round evicts it
// and spawns a fresh one instead of retrying the dead one forever.
func TestRound_FailedReuseFallsBackToFreshProcess(t *testing.T) {
	a := testAgent(t, "pin")
	token := "tok-pin-fallback"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{})
	defer vetting.UnregisterAdvisorThread(token)

	roundOnce := func() (string, error) {
		var specs []eventSpec
		err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "go", artifactsrc.Artifact{}, "", "", token, "", func(s eventSpec) bool {
			specs = append(specs, s)
			return true
		})
		if err != nil {
			return "", err
		}
		return specs[len(specs)-1].parts[0].Text, nil
	}

	first, err := roundOnce()
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if first != "round:1 session:s1" {
		t.Fatalf("round 1 = %q, want round:1 session:s1", first)
	}

	// Simulate the pinned process dying between rounds (crash/OOM) - kill it
	// out from under the registry without going through ClosePinnedSession.
	v, ok := pinned.Load(token)
	if !ok {
		t.Fatal("round 1 did not pin a process")
	}
	if err := v.(*pinnedProc).h.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill pinned process: %v", err)
	}
	v.(*pinnedProc).h.cmd.Wait()

	if _, err := roundOnce(); err == nil {
		t.Fatal("round 2 against a dead pinned process: want an error, not a silent hang or success")
	}
	if _, ok := pinned.Load(token); ok {
		t.Fatal("round 2's failed reuse must evict the dead process from the registry")
	}

	third, err := roundOnce()
	if err != nil {
		t.Fatalf("round 3 (fresh process fallback): %v", err)
	}
	if third != "round:1 session:s1" {
		t.Fatalf("round 3 = %q, want round:1 session:s1 (a BRAND NEW process, counter reset)", third)
	}
}

// TestRound_MCPToolsBlockLeadsThePrompt: what the subprocess receives opens with the exact generated
// tool names for a registered review session.
func TestRound_MCPToolsBlockLeadsThePrompt(t *testing.T) {
	a := testAgent(t, "echo")
	secret, err := vetting.NewMemSecret()
	if err != nil {
		t.Fatal(err)
	}
	vetting.RegisterMemSession(secret, vetting.MemSession{Review: &vetting.ReviewStage{}})
	defer vetting.UnregisterMemSession(secret)

	var specs []eventSpec
	err = a.round(context.Background(), t.TempDir(), secret, workspace.Caps{}, "review this PR", artifactsrc.Artifact{}, "", "", "", "", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	got := specs[len(specs)-1].parts[0].Text
	if !strings.HasPrefix(got, "MCP tools available to you this round:") {
		t.Fatalf("tools block must lead the round's whole message, got: %q", got)
	}
	for _, want := range []string{"quackmcp_stage_review_comment", "quackmcp_list_review_comments", "quackmcp_unstage_review_comment", "quackmcp_stage_review"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt sent to the subprocess is missing tool name %q: %q", want, got)
		}
	}
}

// TestRunPrompt_EnvironmentBlockTrailsTheTask: the per-round environment block sits after the task,
// so the stable task prefix stays a cache hit across rounds. Drives the full runPrompt path.
func TestRunPrompt_EnvironmentBlockTrailsTheTask(t *testing.T) {
	a := testAgent(t, "echo")
	token := vetting.AdvisorThreadToken("plan-1", "impl1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{NodeID: "impl1", WorkspaceNodeID: "impl1", ChatID: "s1", SessionID: "s1"})
	defer vetting.UnregisterAdvisorThread(token)

	r, err := runner.New(runner.Config{
		AppName: "test", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "add the feature\n\n[[quack:advisor-thread:" + token + "]]"}}}
	var lastText string
	for ev, err := range r.Run(vetting.WithAdvisorToken(t.Context(), token), "u1", "s1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.Text != "" {
				lastText = p.Text
			}
		}
	}
	taskIdx := strings.Index(lastText, "add the feature")
	envIdx := strings.Index(lastText, "<environment_context>")
	if taskIdx < 0 || envIdx < 0 {
		t.Fatalf("prompt missing task text or environment block: %q", lastText)
	}
	if envIdx < taskIdx {
		t.Fatalf("environment block must trail the task text, got: %q", lastText)
	}
}

// TestRunPrompt_EnvironmentBlockDisclosesReadOnly: the round's effective caps (AdvisorTask.ReadOnly,
// resolved per node) reach the environment block, not a.opts.Caps's static default.
func TestRunPrompt_EnvironmentBlockDisclosesReadOnly(t *testing.T) {
	a := testAgent(t, "echo")
	token := vetting.AdvisorThreadToken("plan-1", "review1")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{NodeID: "review1", WorkspaceNodeID: "review1", ChatID: "s1", SessionID: "s1", ReadOnly: true})
	defer vetting.UnregisterAdvisorThread(token)

	r, err := runner.New(runner.Config{
		AppName: "test", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "review the PR\n\n[[quack:advisor-thread:" + token + "]]"}}}
	var lastText string
	for ev, err := range r.Run(vetting.WithAdvisorToken(t.Context(), token), "u1", "s1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.Text != "" {
				lastText = p.Text
			}
		}
	}
	if !strings.Contains(lastText, "filesystem: read-only") {
		t.Fatalf("prompt missing the read-only disclosure line: %q", lastText)
	}
}

// TestRound_MCPToolsBlockSaysNoneWhenNoSurface proves the block is rendered
// (loud) rather than omitted (silent) when the round has no MCP participant.
func TestRound_MCPToolsBlockSaysNoneWhenNoSurface(t *testing.T) {
	a := testAgent(t, "echo")
	var specs []eventSpec
	err := a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "add the feature", artifactsrc.Artifact{}, "", "", "", "", func(s eventSpec) bool {
		specs = append(specs, s)
		return true
	})
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	got := specs[len(specs)-1].parts[0].Text
	if !strings.Contains(got, "MCP tools available to you this round: none.") {
		t.Fatalf("expected an explicit \"none\" tools block, got: %q", got)
	}
}

func TestRound_CancelGraceful(t *testing.T) {
	a := testAgent(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	t0 := time.Now()
	err := a.round(ctx, t.TempDir(), "", workspace.Caps{}, "loop forever", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want context cancellation, got %v", err)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("cancel took %v - subprocess not reaped", d)
	}
}

// A worker that ignores session/cancel entirely must still be reaped by the
// process-group kill within the grace window - the v0.5.2 hang class.
func TestRound_StubbornAgentIsKilled(t *testing.T) {
	old := cancelGrace
	cancelGrace = 300 * time.Millisecond
	defer func() { cancelGrace = old }()

	a := testAgent(t, "stubborn")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	t0 := time.Now()
	err := a.round(ctx, t.TempDir(), "", workspace.Caps{}, "loop forever", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true })
	if err == nil {
		t.Fatal("want an error from a cancelled round")
	}
	if d := time.Since(t0); d > 15*time.Second {
		t.Fatalf("stubborn agent survived %v - group kill failed", d)
	}
}

// A silent round (no updates, prompt RPC never returns) is wedged and must end on the idle timeout,
// not the caller's outer context (the 2h run deadline in production).
func TestRound_IdleTimeout(t *testing.T) {
	oldGrace := cancelGrace
	cancelGrace = 200 * time.Millisecond
	defer func() { cancelGrace = oldGrace }()

	a := testAgent(t, "hang")
	a.opts.IdleTimeout = 150 * time.Millisecond

	result := make(chan error, 1)
	go func() {
		result <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "wedge forever", artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true })
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "wedged") {
			t.Fatalf("want a wedged idle-timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round did not return - idle timeout regression, would otherwise hang on the caller's outer context")
	}
}

// fakeIdleTimer puts round()'s idle watchdog under test control, so reset and fire are exact transitions.
type fakeIdleTimer struct {
	ch     chan time.Time
	resets int32 // atomic
}

func newFakeIdleTimer() *fakeIdleTimer { return &fakeIdleTimer{ch: make(chan time.Time, 1)} }

func (f *fakeIdleTimer) C() <-chan time.Time { return f.ch }
func (f *fakeIdleTimer) Stop() bool          { return true }
func (f *fakeIdleTimer) Reset(time.Duration) bool {
	atomic.AddInt32(&f.resets, 1)
	return true
}

// TestRound_IdleTimeoutResetsOnActivityThenFiresOnSilence: activity comes only from test nudges over
// the steer RPC and the fake timer fires by hand, so there are no timing margins at either end.
func TestRound_IdleTimeoutResetsOnActivityThenFiresOnSilence(t *testing.T) {
	a := testAgent(t, "idle-probe")
	timer := newFakeIdleTimer()
	a.newIdleTimer = func(time.Duration) idleTimer { return timer }

	var forward atomic.Pointer[func(string) bool]
	a.opts.RegisterLiveSteer = func(chatID, nodeID string, fwd func(string) bool) { forward.Store(&fwd) }
	a.opts.UnregisterLiveSteer = func(string, string) {}

	var specs []eventSpec
	result := make(chan error, 1)
	go func() {
		result <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, "stay alive", artifactsrc.Artifact{}, "chat1", "node1", "", "", func(s eventSpec) bool {
			specs = append(specs, s)
			return true
		})
	}()

	waitForForward := func() func(string) bool {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p := forward.Load(); p != nil {
				return *p
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("RegisterLiveSteer never fired - round() did not register the live-steer hook")
		return nil
	}
	fwd := waitForForward()

	waitForResets := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if atomic.LoadInt32(&timer.resets) >= n {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("idle timer was not reset %d time(s) after activity", n)
	}

	// Two rounds of real activity, each driven over the actual host->agent
	// RPC - each must reset the idle timer, proving activity keeps it alive.
	if !fwd("nudge 1") {
		t.Fatal("steer forward 1 failed")
	}
	waitForResets(1)
	if !fwd("nudge 2") {
		t.Fatal("steer forward 2 failed")
	}
	waitForResets(2)

	// No third nudge is coming - the round must still be running (an upper
	// bound only: this can never mask a false pass, only a slow failure).
	select {
	case err := <-result:
		t.Fatalf("round returned early (err=%v) - it should still be waiting on the idle timer", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Fire the fake timer by hand - genuine silence, no wall clock involved.
	timer.ch <- time.Now()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "wedged") {
			t.Fatalf("want a wedged idle-timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round did not return after the idle timer fired")
	}
	if len(specs) != 2 || specs[0].parts[0].Text != "ping" || specs[1].parts[0].Text != "ping" {
		t.Fatalf("got %d activity events, want 2 pings: %+v", len(specs), specs)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New("x", "d", Options{}); err == nil {
		t.Fatal("empty command must be rejected")
	}
}

// Permission asks route to the safety judge; no judge ⇒ allow (container
// boundary posture). The chosen option kind follows the verdict.
func TestRequestPermission_JudgeRouting(t *testing.T) {
	opts := []sdk.PermissionOption{
		{Kind: sdk.PermissionOptionKindAllowOnce, OptionId: "yes"},
		{Kind: sdk.PermissionOptionKindRejectOnce, OptionId: "no"},
	}
	kind := sdk.ToolKindRead
	title := "grep /workspace/sibling"
	req := sdk.RequestPermissionRequest{Options: opts, ToolCall: sdk.ToolCallUpdate{ToolCallId: "t1", Kind: &kind, Title: &title}}

	pick := func(judge func(context.Context, string, string, map[string]any) (bool, string)) string {
		h := &clientHandler{judge: judge}
		resp, err := h.RequestPermission(context.Background(), req)
		if err != nil || resp.Outcome.Selected == nil {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
		return string(resp.Outcome.Selected.OptionId)
	}

	if got := pick(nil); got != "yes" {
		t.Fatalf("nil judge must allow, picked %q", got)
	}
	if got := pick(func(context.Context, string, string, map[string]any) (bool, string) { return false, "escape" }); got != "no" {
		t.Fatalf("denying judge must reject, picked %q", got)
	}
	if got := pick(func(context.Context, string, string, map[string]any) (bool, string) { return true, "fine" }); got != "yes" {
		t.Fatalf("allowing judge must allow, picked %q", got)
	}
}

// TestRunPrompt_RemovesScratchDirAfterRound: the per-node scratch dir (the child's TMPDIR)
// does not outlive the round.
func TestRunPrompt_RemovesScratchDirAfterRound(t *testing.T) {
	a := testAgent(t, "echo")
	token := vetting.AdvisorThreadToken("plan-1", "impl-scratch")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{NodeID: "impl-scratch", WorkspaceNodeID: "impl-scratch", ChatID: "s1", SessionID: "s1"})
	defer vetting.UnregisterAdvisorThread(token)

	scratch, err := a.opts.Jail.ScratchDir("u1", "s1", "impl-scratch")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: a, SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "add the feature\n\n[[quack:advisor-thread:" + token + "]]"}}}
	for _, err := range r.Run(vetting.WithAdvisorToken(t.Context(), token), "u1", "s1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if _, statErr := os.Stat(scratch); !os.IsNotExist(statErr) {
		t.Errorf("scratch dir %q must be removed after the round, stat err = %v", scratch, statErr)
	}
}

// TestRound_ReapsChildThatStopsReadingStdin: a child that stops reading stdin wedges the prompt write
// on writeMu; the idle watchdog must still end the round and the deferred close must reap the process.
func TestRound_ReapsChildThatStopsReadingStdin(t *testing.T) {
	old := cancelGrace
	cancelGrace = 300 * time.Millisecond
	defer func() { cancelGrace = old }()

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New("code-implementer", "external coder", Options{
		Command:     []string{os.Args[0]},
		Env:         []string{"QUACK_ACP_FAKE=deaf"},
		Home:        t.TempDir(),
		Jail:        jail,
		UserID:      "u1",
		IdleTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	big := strings.Repeat("x", 512*1024) // > the 64KiB pipe buffer
	result := make(chan error, 1)
	go func() {
		result <- a.round(context.Background(), t.TempDir(), "", workspace.Caps{}, big, artifactsrc.Artifact{}, "", "", "", "", func(eventSpec) bool { return true })
	}()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "wedged") {
			t.Fatalf("want a wedged idle-timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round() never returned - a deaf child wedges gracefulCancel on the same writeMu the prompt write holds, so the child is never reaped")
	}
}
