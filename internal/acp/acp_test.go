package acp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestMain doubles as the fake ACP agent: the tests re-exec the test binary
// with QUACK_ACP_FAKE set, and this intercept runs the agent side of the
// protocol over stdio instead of the test suite — a real subprocess round
// with no external dependency.
func TestMain(m *testing.M) {
	if mode := os.Getenv("QUACK_ACP_FAKE"); mode != "" {
		runFakeAgent(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeAgent(mode string) {
	ag := &fakeAgent{mode: mode}
	conn := sdk.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	ag.conn = conn
	<-conn.Done()
}

type fakeAgent struct {
	mode string
	conn *sdk.AgentSideConnection
}

func (f *fakeAgent) Initialize(ctx context.Context, _ sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{ProtocolVersion: sdk.ProtocolVersionNumber}, nil
}

func (f *fakeAgent) NewSession(ctx context.Context, _ sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	return sdk.NewSessionResponse{SessionId: "s1"}, nil
}

func (f *fakeAgent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	switch f.mode {
	case "hang":
		// Cooperative: the SDK cancels this ctx on session/cancel.
		<-ctx.Done()
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	case "stubborn":
		// Ignores cancellation entirely — only the process-group kill ends it
		// (the v0.5.2 hang class).
		select {}
	}
	send := func(u sdk.SessionUpdate) {
		_ = f.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: p.SessionId, Update: u})
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
	err := a.round(context.Background(), t.TempDir(), "", "add the feature", func(s eventSpec) bool {
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

func TestRound_CancelGraceful(t *testing.T) {
	a := testAgent(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	t0 := time.Now()
	err := a.round(ctx, t.TempDir(), "", "loop forever", func(eventSpec) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want context cancellation, got %v", err)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("cancel took %v — subprocess not reaped", d)
	}
}

// A worker that ignores session/cancel entirely must still be reaped by the
// process-group kill within the grace window — the v0.5.2 hang class.
func TestRound_StubbornAgentIsKilled(t *testing.T) {
	old := cancelGrace
	cancelGrace = 300 * time.Millisecond
	defer func() { cancelGrace = old }()

	a := testAgent(t, "stubborn")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	t0 := time.Now()
	err := a.round(ctx, t.TempDir(), "", "loop forever", func(eventSpec) bool { return true })
	if err == nil {
		t.Fatal("want an error from a cancelled round")
	}
	if d := time.Since(t0); d > 15*time.Second {
		t.Fatalf("stubborn agent survived %v — group kill failed", d)
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
