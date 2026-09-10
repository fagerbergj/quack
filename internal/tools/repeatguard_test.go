package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

type echoArgs struct {
	Q string `json:"q"`
}
type echoResult struct {
	Out string `json:"out"`
}

func newRepeatTestTool(t *testing.T, calls *int) runnableTool {
	return newNamedRepeatTestTool(t, "echo", calls)
}

func newNamedRepeatTestTool(t *testing.T, name string, calls *int) runnableTool {
	t.Helper()
	tl, err := functiontool.New[echoArgs, echoResult](
		functiontool.Config{Name: name, Description: "echo"},
		func(_ adkagent.Context, a echoArgs) (echoResult, error) {
			*calls++
			return echoResult{Out: a.Q}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl.(runnableTool)
}

type pathArgs struct {
	Path string `json:"path"`
	Note string `json:"note,omitempty"` // varies call-to-call without changing Path
}
type pathResult struct {
	Out string `json:"out"`
}

// newFailingPathTool returns a `path`-taking tool whose calls fail (return an
// error) when fail reports true for that call's args, so tests can simulate
// an agent varying its arguments while retrying against the same resource.
func newFailingPathTool(t *testing.T, calls *int, fail func(pathArgs) bool) runnableTool {
	t.Helper()
	tl, err := functiontool.New[pathArgs, pathResult](
		functiontool.Config{Name: "read_thing", Description: "read a thing"},
		func(_ adkagent.Context, a pathArgs) (pathResult, error) {
			*calls++
			if fail(a) {
				return pathResult{}, fmt.Errorf("not found: %s", a.Path)
			}
			return pathResult{Out: a.Path}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl.(runnableTool)
}

// Semantic churn: consecutive calls against the same path, each with a
// different `note` (never byte-identical args, so the exact-match guard never
// trips); all fail, and once pathFailThreshold (3) have run and failed the next attempt is refused before the tool even runs.
func TestRepeatGuardCatchesSemanticChurn(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newFailingPathTool(t, &calls, func(pathArgs) bool { return true }), newRepeatStates(), 3, 3, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")

	notes := []string{"try 1", "try two", "3rd attempt", "one more try"}
	for i, note := range notes {
		_, runErr := rg.Run(ctx, map[string]any{"path": "src/main.go", "note": note})
		if i < pathFailThreshold {
			if runErr == nil {
				t.Fatalf("call %d: want the underlying tool error, got nil", i+1)
			}
			if strings.Contains(runErr.Error(), "REFUSED") {
				t.Fatalf("call %d: refused too early: %v", i+1, runErr)
			}
			continue
		}
		if runErr == nil || !strings.Contains(runErr.Error(), "REFUSED") {
			t.Fatalf("call %d after %d consecutive failures: want REFUSED, got %v", i+1, pathFailThreshold, runErr)
		}
	}
	if calls != pathFailThreshold {
		t.Fatalf("tool executed %d times; want %d (last call refused before running)", calls, pathFailThreshold)
	}
}

// Genuinely different calls - different resources, or a call that succeeds -
// are never caught: failures against different paths don't share a streak,
// and a success resets the streak for its own path.
func TestRepeatGuardResourceFailAllowsGenuineDifference(t *testing.T) {
	calls := 0
	states := newRepeatStates()

	// Two different paths, each failing twice: neither reaches the threshold.
	g1, _ := newRepeatGuard(newFailingPathTool(t, &calls, func(pathArgs) bool { return true }), states, 3, 3, 0, nil)
	rg1 := g1.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	for i, note := range []string{"a", "b"} {
		if _, err := rg1.Run(ctx, map[string]any{"path": "one.go", "note": note}); err == nil || strings.Contains(err.Error(), "REFUSED") {
			t.Fatalf("one.go call %d: want plain tool failure, got %v", i+1, err)
		}
	}
	for i, note := range []string{"a", "b"} {
		if _, err := rg1.Run(ctx, map[string]any{"path": "two.go", "note": note}); err == nil || strings.Contains(err.Error(), "REFUSED") {
			t.Fatalf("two.go call %d: want plain tool failure, got %v", i+1, err)
		}
	}

	// A path whose 3rd call succeeds resets the streak: two more failures
	// afterward must not be refused (only 2 consecutive since the reset).
	n := 0
	g2, _ := newRepeatGuard(newFailingPathTool(t, &n, func(a pathArgs) bool { return a.Note != "fixed" }), states, 3, 3, 0, nil)
	rg2 := g2.(*repeatGuard)
	seq := []string{"x", "y", "fixed", "z", "w"}
	for i, note := range seq {
		if _, err := rg2.Run(ctx, map[string]any{"path": "three.go", "note": note}); err != nil && strings.Contains(err.Error(), "REFUSED") {
			t.Fatalf("three.go call %d (%s): unexpected refusal: %v", i+1, note, err)
		}
	}
}

// repeatCtx mirrors cd_test.go's fakeCtx surface (functiontool.Run touches
// more of Context than just SessionID), with a configurable session id.
type repeatCtx struct {
	adkagent.StrictContextMock
	sid     string
	state   *fakeState
	content *genai.Content
}

func (c *repeatCtx) UserContent() *genai.Content                          { return c.content }
func (c *repeatCtx) InvocationID() string                                 { return "inv" }
func (c *repeatCtx) AgentName() string                                    { return "test" }
func (c *repeatCtx) UserID() string                                       { return "u" }
func (c *repeatCtx) AppName() string                                      { return "app" }
func (c *repeatCtx) SessionID() string                                    { return c.sid }
func (c *repeatCtx) Branch() string                                       { return "" }
func (c *repeatCtx) Artifacts() adkagent.Artifacts                        { return nil }
func (c *repeatCtx) State() session.State                                 { return c.state }
func (c *repeatCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (c *repeatCtx) ReadonlyState() session.ReadonlyState                 { return c.state }

func newRepeatCtx(sid string) *repeatCtx {
	return &repeatCtx{StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()}, sid: sid, state: &fakeState{m: map[string]any{}}}
}

// newRepeatCtxWithAdvisorThread is newRepeatCtx plus the advisor-thread
// marker nodeScope resolves (chatID, nodeID) from - what a worker's tool
// context inside a gated DAG node actually carries (see gatedCtx in
// nodescope_test.go, which uses a different fake Context surface than the
// one functiontool.Run needs here).
func newRepeatCtxWithAdvisorThread(t *testing.T, sid, chatID, nodeID string) *repeatCtx {
	t.Helper()
	token := vetting.AdvisorThreadToken("plan-1", nodeID)
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: chatID, SessionID: sid, NodeID: nodeID})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	c := newRepeatCtx(sid)
	c.content = &genai.Content{Parts: []*genai.Part{{Text: "do the task\n\n" + vetting.AdvisorThreadMarker(token)}}}
	return c
}

// The breaker: with the threshold set to 3, the 1st and 2nd identical calls
// run; the 3rd is refused (tool NOT executed) with a steering error; the 4th
// is where the hard stop fires and ends the node's turn.
func TestRepeatGuardRefusesThirdIdenticalCall(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), 3, 3, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same"}

	for i := 1; i <= 2; i++ {
		if _, err := rg.Run(ctx, args); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err3 := rg.Run(ctx, args)
	if err3 == nil || !strings.Contains(err3.Error(), "REFUSED") {
		t.Fatalf("3rd identical call: want REFUSED, got %v", err3)
	}
	_, err4 := rg.Run(ctx, args)
	if err4 == nil || !strings.Contains(err4.Error(), "tool-call loop") {
		t.Fatalf("4th identical call: want the hard-stop error, got %v", err4)
	}
	if calls != 2 {
		t.Fatalf("tool executed %d times; want 2 (refusals must not execute)", calls)
	}
}

// Two-tier default thresholds: a streak of SUCCESSFUL identical calls gets
// more leash (successThreshold=8) than one that keeps failing/being refused
// (failThreshold=3) - a legitimate idempotent re-check shouldn't trip as
// eagerly as a call that's visibly not working.
func TestRepeatGuardSuccessStreakUsesHigherThreshold(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), defaultFailThreshold, defaultSuccessThreshold, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same"}

	for i := 1; i < defaultSuccessThreshold; i++ {
		if _, err := rg.Run(ctx, args); err != nil {
			t.Fatalf("call %d: want a plain success (below the success threshold), got %v", i, err)
		}
	}
	_, refused := rg.Run(ctx, args)
	if refused == nil || !strings.Contains(refused.Error(), "REFUSED") {
		t.Fatalf("call %d: want REFUSED at the success threshold, got %v", defaultSuccessThreshold, refused)
	}
	if calls != defaultSuccessThreshold-1 {
		t.Fatalf("tool executed %d times; want %d", calls, defaultSuccessThreshold-1)
	}
}

// The hard stop: once a call is refused, repeating it again reaches
// dag.Executor.NoteToolLoopFailure (via endTurn) with the offending tool name
// and count, in addition to returning a terminal error - the loop must end
// the node's turn, not just keep refusing forever (the pre-existing bug).
func TestRepeatGuardHardStopsAfterRefusal(t *testing.T) {
	calls := 0
	var gotChat, gotNode, gotMsg string
	endTurn := func(chatID, nodeID, msg string) bool {
		gotChat, gotNode, gotMsg = chatID, nodeID, msg
		return true
	}
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), 2, 2, 0, endTurn)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtxWithAdvisorThread(t, "s1", "chat-1", "node-1")
	args := map[string]any{"q": "same"}

	if _, err := rg.Run(ctx, args); err != nil {
		t.Fatalf("1st call: %v", err)
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("2nd (threshold) call: want REFUSED, got %v", err)
	}
	if gotMsg != "" {
		t.Fatalf("endTurn fired before the hard-stop repeat: %q", gotMsg)
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "tool-call loop") {
		t.Fatalf("3rd call: want the hard-stop error, got %v", err)
	}
	if gotChat != "chat-1" || gotNode != "node-1" {
		t.Fatalf("endTurn got (%q, %q); want (chat-1, node-1)", gotChat, gotNode)
	}
	if !strings.Contains(gotMsg, "echo") || !strings.Contains(gotMsg, "3") {
		t.Fatalf("endTurn message missing tool name/count: %q", gotMsg)
	}
	if calls != 1 {
		t.Fatalf("tool executed %d times; want 1 (only the first call runs)", calls)
	}
}

// The total-call ceiling is a backstop independent of any single fingerprint
// - varying the args every call never trips the identical-call guard, but
// still hits the ceiling and ends the node's turn.
func TestRepeatGuardTotalCallCeiling(t *testing.T) {
	calls := 0
	var endedMsg string
	endTurn := func(chatID, nodeID, msg string) bool { endedMsg = msg; return true }
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), defaultFailThreshold, defaultSuccessThreshold, 3, endTurn)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtxWithAdvisorThread(t, "s1", "chat-1", "node-1")

	for i := 1; i <= 3; i++ {
		if _, err := rg.Run(ctx, map[string]any{"q": fmt.Sprintf("distinct-%d", i)}); err != nil {
			t.Fatalf("call %d: want success (varying args, under the ceiling), got %v", i, err)
		}
	}
	_, err = rg.Run(ctx, map[string]any{"q": "distinct-4"})
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("call over the ceiling: want a ceiling error, got %v", err)
	}
	if endedMsg == "" {
		t.Fatal("endTurn never fired for the total-call ceiling")
	}
	if calls != 3 {
		t.Fatalf("tool executed %d times; want 3", calls)
	}
}

// Different args, a different tool in between, or a different session all
// reset consecutiveness - A,B,A is not a repeat.
func TestRepeatGuardResets(t *testing.T) {
	calls := 0
	states := newRepeatStates()
	g, _ := newRepeatGuard(newRepeatTestTool(t, &calls), states, 3, 3, 0, nil)
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")

	// A, A, B, A, A: never three consecutive identical - all run.
	seq := []map[string]any{
		{"q": "a"}, {"q": "a"}, {"q": "b"}, {"q": "a"}, {"q": "a"},
	}
	for i, a := range seq {
		if _, err := rg.Run(ctx, a); err != nil {
			t.Fatalf("call %d (%v): %v", i+1, a, err)
		}
	}
	// Another tool's call between repeats resets too (shared states).
	other := 0
	g2, _ := newRepeatGuard(newNamedRepeatTestTool(t, "echo2", &other), states, 3, 3, 0, nil)
	if _, err := g2.(*repeatGuard).Run(ctx, map[string]any{"q": "a"}); err != nil {
		t.Fatalf("other tool: %v", err)
	}
	if _, err := rg.Run(ctx, map[string]any{"q": "a"}); err != nil {
		t.Fatalf("after other tool, identical call must run: %v", err)
	}
	// A different session is independent.
	if _, err := rg.Run(newRepeatCtx("s2"), map[string]any{"q": "a"}); err != nil {
		t.Fatalf("fresh session: %v", err)
	}
	if calls != 7 {
		t.Fatalf("tool executed %d times; want 7", calls)
	}
}
