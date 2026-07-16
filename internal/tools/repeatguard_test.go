package tools

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
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

// repeatCtx mirrors cd_test.go's fakeCtx surface (functiontool.Run touches
// more of Context than just SessionID), with a configurable session id.
type repeatCtx struct {
	adkagent.StrictContextMock
	sid   string
	state *fakeState
}

func (c *repeatCtx) UserContent() *genai.Content                          { return nil }
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

// The breaker: 1st and 2nd identical calls run; the 3rd is refused with a
// steering error (and the tool is NOT executed); the refusal text carries the
// attempt counter so consecutive refusals are never byte-identical results.
func TestRepeatGuardRefusesThirdIdenticalCall(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates())
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
	if err3 == nil || !strings.Contains(err3.Error(), "REFUSED (attempt 1)") {
		t.Fatalf("3rd identical call: want REFUSED attempt 1, got %v", err3)
	}
	_, err4 := rg.Run(ctx, args)
	if err4 == nil || !strings.Contains(err4.Error(), "REFUSED (attempt 2)") {
		t.Fatalf("4th identical call: want REFUSED attempt 2 (non-identical refusal text), got %v", err4)
	}
	if calls != 2 {
		t.Fatalf("tool executed %d times; want 2 (refusals must not execute)", calls)
	}
}

// Different args, a different tool in between, or a different session all
// reset consecutiveness — A,B,A is not a repeat.
func TestRepeatGuardResets(t *testing.T) {
	calls := 0
	states := newRepeatStates()
	g, _ := newRepeatGuard(newRepeatTestTool(t, &calls), states)
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")

	// A, A, B, A, A: never three consecutive identical — all run.
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
	g2, _ := newRepeatGuard(newNamedRepeatTestTool(t, "echo2", &other), states)
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
