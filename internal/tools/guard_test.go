package tools

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ── unit: parseGuardTier ─────────────────────────────────────────────────────

func TestParseGuardTier(t *testing.T) {
	cases := []struct {
		in      string
		tier    guardTier
		guarded bool
	}{
		{"judge", guardTier{Judge: true}, true},
		{"confirm", guardTier{Confirm: true}, true},
		{"judge+confirm", guardTier{Judge: true, Confirm: true}, true},
		{"JUDGE", guardTier{Judge: true}, true}, // defensive case-insensitivity
		{"none", guardTier{}, false},
		{"", guardTier{}, false},
		{"yolo", guardTier{}, false}, // config.Load rejects this at startup; here it degrades to unguarded
	}
	for _, c := range cases {
		tier, guarded := parseGuardTier(c.in)
		if tier != c.tier || guarded != c.guarded {
			t.Errorf("parseGuardTier(%q) = (%+v, %v), want (%+v, %v)", c.in, tier, guarded, c.tier, c.guarded)
		}
	}
}

// ── unit: the judge tier (deny short-circuits, allow executes, missing judge
//    fails closed) ────────────────────────────────────────────────────────────

// fakeRunnable is a hand-rolled runnableTool that records executions - used
// instead of a functiontool so unit tests need no agent.Context plumbing.
type fakeRunnable struct {
	mu   sync.Mutex
	runs int
}

func (*fakeRunnable) Name() string        { return "risky_op" }
func (*fakeRunnable) Description() string { return "a risky operation" }
func (*fakeRunnable) IsLongRunning() bool { return false }
func (*fakeRunnable) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "risky_op"}
}
func (*fakeRunnable) ProcessRequest(adkagent.Context, *model.LLMRequest) error { return nil }
func (f *fakeRunnable) Run(adkagent.Context, any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return map[string]any{"ok": true}, nil
}
func (f *fakeRunnable) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func TestGuardJudgeDenyReturnsRefusalWithoutExecuting(t *testing.T) {
	inner := &fakeRunnable{}
	deny := func(_ context.Context, _, _, toolName string, _ map[string]any, _ string) (bool, string, error) {
		return false, "not in service of the task", nil
	}
	g, err := newGuardedTool(inner, guardTier{Judge: true}, deny, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.(*guardedTool).Run(nil, map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("Run: %v (a denial must be a RESULT, not an error)", err)
	}
	if res[vetting.GuardStatusKey] != "denied" {
		t.Errorf("result = %v, want status denied", res)
	}
	if reason, _ := res["reason"].(string); !strings.Contains(reason, "not in service") {
		t.Errorf("reason = %q, want the judge's reason", reason)
	}
	if inner.runCount() != 0 {
		t.Errorf("inner ran %d times, want 0 - a denied tool must never execute", inner.runCount())
	}
}

func TestGuardJudgeAllowExecutes(t *testing.T) {
	inner := &fakeRunnable{}
	allow := func(_ context.Context, _, _, _ string, _ map[string]any, _ string) (bool, string, error) {
		return true, "on task", nil
	}
	g, err := newGuardedTool(inner, guardTier{Judge: true}, allow, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.(*guardedTool).Run(nil, map[string]any{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res["ok"] != true {
		t.Errorf("result = %v, want the inner tool's result", res)
	}
	if inner.runCount() != 1 {
		t.Errorf("inner ran %d times, want 1", inner.runCount())
	}
}

func TestGuardJudgeUnavailableFailsClosed(t *testing.T) {
	inner := &fakeRunnable{}
	g, err := newGuardedTool(inner, guardTier{Judge: true}, nil, nil) // no SafetyJudge configured
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.(*guardedTool).Run(nil, map[string]any{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res[vetting.GuardStatusKey] != "denied" {
		t.Errorf("result = %v, want a fail-closed denial when no judge is configured", res)
	}
	if inner.runCount() != 0 {
		t.Errorf("inner ran %d times, want 0", inner.runCount())
	}
}

// ── unit: Build applies the wrapper at registration time ────────────────────

func TestBuildWrapsGuardedTools(t *testing.T) {
	tools, err := Build([]string{"ask_user", "current_date"}, Deps{
		Guards: map[string]string{"ask_user": "judge"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Build always adds the repeat guard as the outer layer (repeatguard.go);
	// the guard-ladder wrapper sits inside it.
	rg0, ok := tools[0].(*repeatGuard)
	if !ok {
		t.Fatalf("ask_user = %T, want *repeatGuard(outer)", tools[0])
	}
	if _, ok := rg0.inner.(*guardedTool); !ok {
		t.Errorf("ask_user (guards: judge) inner = %T, want *guardedTool", rg0.inner)
	}
	rg1, ok := tools[1].(*repeatGuard)
	if !ok {
		t.Fatalf("current_date = %T, want *repeatGuard(outer)", tools[1])
	}
	if _, ok := rg1.inner.(*guardedTool); ok {
		t.Error("current_date (unlisted) must NOT be guard-laddered")
	}
}

// ── integration: the confirm tier pauses the NODE via the adk_request_
//    confirmation marker + the existing HITL park, and resumes on the human's
//    decision (mirrors internal/dag/hitl_test.go's pause/resume pattern). ────

// confirmStub drives the worker + the vetting judge:
//   - judge requests (submit_verdict tool present) always pass;
//   - a request whose history already carries the guarded tool's RESOLVED
//     response (post-approval execution) → final answer;
//   - a post-decision prompt saying APPROVED → re-issue the risky_op call;
//   - a post-decision prompt saying DENIED → answer without the operation;
//   - otherwise (fresh draft) → propose risky_op.
type confirmStub struct{}

func (*confirmStub) Name() string { return "confirmStub" }

func (s *confirmStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if reqHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if reqHasResolvedResponse(req, "risky_op") {
			yield(atText("FINAL: operation performed."), nil)
			return
		}
		txt := atAllText(req)
		switch {
		case strings.Contains(txt, "APPROVED"):
			yield(atCall("risky_op", map[string]any{"target": "x"}), nil)
		case strings.Contains(txt, "DENIED"):
			yield(atText("FINAL: completed without the operation."), nil)
		default:
			yield(atCall("risky_op", map[string]any{"target": "x"}), nil)
		}
	}
}

func reqHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

// reqHasResolvedResponse reports whether the request's history carries a
// FunctionResponse for name marked with vetting.GuardResolvedKey - i.e. the
// guarded tool already executed for real this round.
func reqHasResolvedResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != name {
				continue
			}
			if v, ok := p.FunctionResponse.Response[vetting.GuardResolvedKey].(bool); ok && v {
				return true
			}
		}
	}
	return false
}

// newConfirmHarness builds a one-node plan whose worker carries a REAL
// confirm-guarded tool (built by this package), run by the REAL dag executor -
// so the pause rides the production plumbing end to end.
func newConfirmHarness(t *testing.T) (*dag.Executor, dag.Plan, session.Service, *fakeRunnable) {
	t.Helper()
	sessions := session.InMemoryService()
	inner := &fakeRunnable{}
	guarded, err := newGuardedTool(inner, guardTier{Confirm: true}, nil, sessions)
	if err != nil {
		t.Fatal(err)
	}
	stub := &confirmStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Do the work.",
		Tools: []tool.Tool{guarded},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": worker}, nil,
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := dag.Plan{ID: "t", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}
	return ex, plan, sessions, inner
}

// sessionHasConfirmationCall asserts the ADK-native adk_request_confirmation
// FunctionCall landed in the session (the wire marker the design doc names).
func sessionHasConfirmationCall(t *testing.T, sessions session.Service, appName, userID, sessID string) bool {
	t.Helper()
	resp, err := sessions.Get(context.Background(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: sessID})
	if err != nil || resp == nil || resp.Session == nil {
		t.Fatalf("session get: %v", err)
	}
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == toolconfirmation.FunctionCallName {
				return true
			}
		}
	}
	return false
}

func runConfirmTurn(t *testing.T, ex *dag.Executor, plan dag.Plan, content *genai.Content, resume []string) (map[string]string, bool, string, string) {
	t.Helper()
	var pauseID, pauseMsg string
	yield := func(ev stream.SSEEvent, _ error) bool {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			pauseID, pauseMsg = d.InterruptID, d.Message
		}
		return true
	}
	out := map[string]string{}
	paused, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u", "s", content, yield, out, resume)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out, paused, pauseID, pauseMsg
}

func confirmAnswer(interruptID, decision string) *genai.Content {
	return &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{
			ID: interruptID, Name: workflow.WorkflowInputFunctionCallName,
			Response: map[string]any{"payload": decision},
		},
	}}}
}

func TestGuardConfirmTier_PauseApproveResume(t *testing.T) {
	ex, plan, sessions, inner := newConfirmHarness(t)

	// Run 1: the worker proposes risky_op → the guard requests confirmation →
	// the node parks under confirm-n1-r1 with the operation in the message.
	out1, paused, pauseID, pauseMsg := runConfirmTurn(t, ex, plan,
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused || out1["n1"] != "" {
		t.Fatalf("run1: want paused with no output, got paused=%v out=%q", paused, out1["n1"])
	}
	if pauseID != "confirm-n1-r1" {
		t.Fatalf("run1: interrupt = %q, want confirm-n1-r1", pauseID)
	}
	if !strings.Contains(pauseMsg, "risky_op") {
		t.Errorf("run1: pause message %q does not name the operation", pauseMsg)
	}
	if inner.runCount() != 0 {
		t.Fatalf("run1: inner ran %d times before approval, want 0", inner.runCount())
	}
	if !sessionHasConfirmationCall(t, sessions, "quack", "u", "s") {
		t.Error("run1: no adk_request_confirmation FunctionCall in the session - the ADK-native marker is missing")
	}

	// Run 2: the human approves → the node resumes, the worker re-issues the
	// call, the guard executes it for real, and the node completes.
	out2, paused2, _, _ := runConfirmTurn(t, ex, plan, confirmAnswer(pauseID, "approve"), []string{"n1"})
	if paused2 {
		t.Fatal("run2: still paused after approval")
	}
	if !strings.Contains(out2["n1"], "operation performed") {
		t.Errorf("run2: out = %q, want the post-execution answer", out2["n1"])
	}
	if inner.runCount() != 1 {
		t.Errorf("run2: inner ran %d times, want exactly 1", inner.runCount())
	}
}

func TestGuardConfirmTier_PauseDenyResume(t *testing.T) {
	ex, plan, _, inner := newConfirmHarness(t)

	_, paused, pauseID, _ := runConfirmTurn(t, ex, plan,
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused {
		t.Fatal("run1: expected a pause")
	}

	out2, paused2, _, _ := runConfirmTurn(t, ex, plan, confirmAnswer(pauseID, "deny"), []string{"n1"})
	if paused2 {
		t.Fatal("run2: still paused after denial")
	}
	if !strings.Contains(out2["n1"], "without the operation") {
		t.Errorf("run2: out = %q, want the without-the-operation answer", out2["n1"])
	}
	if inner.runCount() != 0 {
		t.Errorf("run2: inner ran %d times after denial, want 0 - a denied operation must never execute", inner.runCount())
	}
}

// pinStub drives the args-pinning scenario: it proposes risky_op(target:x),
// and after the human APPROVES it re-issues the call with DIFFERENT args
// (target:EVIL) - modeling a steered/injected model swapping the operation
// after approval. After the swapped call's own confirmation is DENIED, it
// re-issues the ORIGINAL approved call (target:x), which must still consume
// the original approval.
type pinStub struct{}

func (*pinStub) Name() string { return "pinStub" }

func (s *pinStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if reqHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if reqHasResolvedResponse(req, "risky_op") {
			yield(atText("FINAL: operation performed."), nil)
			return
		}
		txt := atAllText(req)
		switch {
		case strings.Contains(txt, "DENIED"):
			// Round 3: the swapped-args op was denied; retry the ORIGINAL one.
			yield(atCall("risky_op", map[string]any{"target": "x"}), nil)
		case strings.Contains(txt, "APPROVED"):
			// Round 2: approval in hand - but swap the arguments.
			yield(atCall("risky_op", map[string]any{"target": "EVIL"}), nil)
		default:
			yield(atCall("risky_op", map[string]any{"target": "x"}), nil)
		}
	}
}

// TestGuardConfirmTier_ApprovalPinnedToArgs: an approval is pinned to the
// exact operation the human saw. A re-issued call with different arguments
// must NOT consume it (and must not execute) - it becomes a fresh proposal
// whose confirmation warns it DIFFERS - while the original approval stays
// available for a later same-args call.
func TestGuardConfirmTier_ApprovalPinnedToArgs(t *testing.T) {
	sessions := session.InMemoryService()
	inner := &fakeRunnable{}
	guarded, err := newGuardedTool(inner, guardTier{Confirm: true}, nil, sessions)
	if err != nil {
		t.Fatal(err)
	}
	stub := &pinStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Do the work.",
		Tools: []tool.Tool{guarded},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": worker}, nil,
		vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := dag.Plan{ID: "t", UserMessage: "x", Nodes: []dag.Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	// Run 1: propose risky_op(target:x) → pause r1.
	_, paused, pauseID, _ := runConfirmTurn(t, ex, plan,
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}, nil)
	if !paused || pauseID != "confirm-n1-r1" {
		t.Fatalf("run1: want pause confirm-n1-r1, got paused=%v id=%q", paused, pauseID)
	}

	// Run 2: human APPROVES target:x - the worker re-issues with target:EVIL.
	// The swapped call must NOT execute and must raise a FRESH confirmation
	// whose message says it DIFFERS.
	out2, paused2, pauseID2, pauseMsg2 := runConfirmTurn(t, ex, plan, confirmAnswer(pauseID, "approve"), []string{"n1"})
	if !paused2 || pauseID2 != "confirm-n1-r2" {
		t.Fatalf("run2: want a second pause confirm-n1-r2, got paused=%v id=%q out=%q", paused2, pauseID2, out2["n1"])
	}
	if inner.runCount() != 0 {
		t.Fatalf("run2: inner ran %d times on swapped args, want 0 - the approval must be pinned", inner.runCount())
	}
	if !strings.Contains(pauseMsg2, "DIFFERS") {
		t.Errorf("run2: pause message %q does not warn the operation DIFFERS from the approved one", pauseMsg2)
	}
	if !strings.Contains(pauseMsg2, "EVIL") {
		t.Errorf("run2: pause message %q does not show the swapped arguments", pauseMsg2)
	}

	// Run 3: human DENIES the swapped op. The worker retries the ORIGINAL
	// target:x call - the round-1 approval is still unconsumed and pinned to
	// exactly those args, so it executes now, exactly once.
	out3, paused3, _, _ := runConfirmTurn(t, ex, plan, confirmAnswer(pauseID2, "deny"), []string{"n1"})
	if paused3 {
		t.Fatal("run3: still paused after the denial")
	}
	if !strings.Contains(out3["n1"], "operation performed") {
		t.Errorf("run3: out = %q, want the post-execution answer", out3["n1"])
	}
	if inner.runCount() != 1 {
		t.Errorf("run3: inner ran %d times, want exactly 1 (the original approved operation)", inner.runCount())
	}
}

// ── unit: the safety-judge prompt carries every context section ─────────────

func TestBuildSafetyJudgePrompt(t *testing.T) {
	p := buildSafetyJudgePrompt("find the bug", "fix pkg X", "delete_path", map[string]any{"path": "repo"}, "  - read_file")
	for _, want := range []string{"find the bug", "fix pkg X", "delete_path", `"path":"repo"`, "read_file", "submit_safety_verdict"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}
