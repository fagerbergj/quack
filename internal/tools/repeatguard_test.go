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
	g, err := newRepeatGuard(newFailingPathTool(t, &calls, func(pathArgs) bool { return true }), newRepeatStates(), nil)
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
	g1, _ := newRepeatGuard(newFailingPathTool(t, &calls, func(pathArgs) bool { return true }), states, nil)
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
	g2, _ := newRepeatGuard(newFailingPathTool(t, &n, func(a pathArgs) bool { return a.Note != "fixed" }), states, nil)
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
// content carries the advisor-thread marker nodeScope resolves (chatID,
// nodeID) from when set (newRepeatCtxWithAdvisorThread) - nil by default,
// matching an orchestrator-level call, which is never node-scoped.
type repeatCtx struct {
	adkagent.StrictContextMock
	sid     string
	state   *fakeState
	content *genai.Content
	actions session.EventActions
}

func (c *repeatCtx) Actions() *session.EventActions { return &c.actions }

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
// context inside a gated DAG node actually carries.
func newRepeatCtxWithAdvisorThread(t *testing.T, sid, chatID, nodeID string) *repeatCtx {
	t.Helper()
	token := vetting.AdvisorThreadToken("plan-1", nodeID)
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: chatID, SessionID: sid, NodeID: nodeID})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	c := newRepeatCtx(sid)
	c.content = &genai.Content{Parts: []*genai.Part{{Text: "do the task\n\n" + vetting.AdvisorThreadMarker(token)}}}
	return c
}

// TestRepeatGuardOrchestratorSoftRefuseLeavesTurnOpen pins the owner's
// settled direction for this guard (#slice3 review, a rig regression):
// return the error to the model and let the turn continue - ending it on
// the very first refusal denied the model any chance to correct a mistake
// (e.g. a missing `agent` field) and retry within the same turn. A call
// that is never node-scoped (nodeScope resolves nodeID=="" - the
// orchestrator's own hand-built tools) must NOT touch SkipSummarization on
// a mere soft refusal; only the hard-stop tier
// (TestRepeatGuardEndsOrchestratorTurnOnHardStop) does.
func TestRepeatGuardOrchestratorSoftRefuseLeavesTurnOpen(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1") // no advisor thread: nodeScope resolves nodeID=="", the orchestrator's own shape
	args := map[string]any{"q": "same"}

	for i := 1; i <= 2; i++ {
		if _, err := rg.Run(ctx, args); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("3rd call: want REFUSED, got %v", err)
	}
	if ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = true after a single soft refusal, want the turn left open for the model to correct and retry")
	}

	// The model corrects (different args): the call runs normally, no
	// leftover effect from the earlier refusal.
	if _, err := rg.Run(ctx, map[string]any{"q": "corrected"}); err != nil {
		t.Fatalf("corrected call: want it to run, got %v", err)
	}
	if ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = true after a corrected retry, want untouched")
	}
}

// TestRepeatGuardEndsOrchestratorTurnOnHardStop is
// TestRepeatGuardEndsRoundAfterRefusalIgnored's orchestrator-side twin: a
// call never scoped to a worker node ends the CALLING turn directly via
// SkipSummarization once the model keeps re-issuing the refused call
// regardless (repeatHardStopAfter times past the soft-refusal threshold) -
// not on the first refusal.
func TestRepeatGuardEndsOrchestratorTurnOnHardStop(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same"}

	for i := 1; i <= repeatThreshold+repeatHardStopAfter; i++ {
		rg.Run(ctx, args)
		if i < repeatThreshold+repeatHardStopAfter && ctx.actions.SkipSummarization {
			t.Fatalf("call %d: SkipSummarization set before the hard-stop tier", i)
		}
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "tool-call loop") {
		t.Fatalf("call after %d refusals: want the hard-stop error, got %v", repeatThreshold+repeatHardStopAfter, err)
	}
	if !ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = false after the hard-stop tier, want true - the orchestrator's turn must end")
	}
	if calls != repeatThreshold-1 {
		t.Fatalf("tool executed %d times; want %d (refusals must not execute)", calls, repeatThreshold-1)
	}
}

// A worker-node-scoped call must never touch Actions on a mere refusal - the
// gate's own continuation/give-up logic owns that decision; only the
// hard-stop tier (TestRepeatGuardEndsRoundAfterRefusalIgnored) ends its round, via tripped.
func TestRepeatGuardWorkerNodeRefusalLeavesActionsAlone(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtxWithAdvisorThread(t, "s1", "chat-1", "node-1")
	args := map[string]any{"q": "same"}
	for i := 1; i <= 3; i++ {
		rg.Run(ctx, args)
	}
	if ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = true for a node-scoped refusal, want untouched")
	}
}

// The breaker: 1st and 2nd identical calls run; the 3rd is refused with a
// steering error (and the tool is NOT executed); the refusal text carries the
// attempt counter so consecutive refusals are never byte-identical results.
func TestRepeatGuardRefusesThirdIdenticalCall(t *testing.T) {
	calls := 0
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), nil)
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

// Different args (to the same tool) each keep their own independent
// adjacency and cross-call streaks - A,B,A,B never reaches a 3rd occurrence
// of either - and a different session is always independent, even for the
// exact same args.
func TestRepeatGuardResets(t *testing.T) {
	calls := 0
	states := newRepeatStates()
	g, _ := newRepeatGuard(newRepeatTestTool(t, &calls), states, nil)
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")

	seq := []map[string]any{
		{"q": "a"}, {"q": "b"}, {"q": "a"}, {"q": "b"},
	}
	for i, a := range seq {
		if _, err := rg.Run(ctx, a); err != nil {
			t.Fatalf("call %d (%v): %v", i+1, a, err)
		}
	}
	if _, err := rg.Run(newRepeatCtx("s2"), map[string]any{"q": "a"}); err != nil {
		t.Fatalf("fresh session: %v", err)
	}
	if calls != 5 {
		t.Fatalf("tool executed %d times; want 5", calls)
	}
}

// TestRepeatGuardCountsAcrossInterleavedOtherCalls pins the rig regression
// (#slice3 review): the loop that slipped past the old guard alternated two
// different tools (edit_plan/execute), so neither tool's own calls were ever
// back-to-back. The streak must be keyed per (session, tool, args), not per
// "last call in the session" - so identical calls to ONE tool still add up
// toward refusal even with other tool calls sitting between them. Uses
// "execute" - crossCalls tracking is scoped to the orchestrator's own plan
// tools (crossCallTools); a tool outside that list is covered by
// TestRepeatGuardCrossCallScopedToPlanTools below.
func TestRepeatGuardCountsAcrossInterleavedOtherCalls(t *testing.T) {
	calls := 0
	states := newRepeatStates()
	g, _ := newRepeatGuard(newNamedRepeatTestTool(t, "execute", &calls), states, nil)
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same"}

	other := 0
	og, _ := newRepeatGuard(newNamedRepeatTestTool(t, "edit_plan", &other), states, nil)
	rog := og.(*repeatGuard)

	for i := 1; i <= 2; i++ {
		if _, err := rg.Run(ctx, args); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if _, err := rog.Run(ctx, map[string]any{"q": "interleaved"}); err != nil {
			t.Fatalf("interleaved other-tool call %d: %v", i, err)
		}
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "REFUSED") {
		t.Fatalf("3rd identical call (with other-tool calls between each): want REFUSED, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("tool executed %d times; want 2 (3rd refused before running)", calls)
	}
}

// TestRepeatGuardCrossCallScopedToPlanTools pins the reviewer's blocker
// (#slice3 review): a tool outside crossCallTools (a worker's read_file,
// read_artifact, ...) must never be cross-refused, even on an identical
// (session, tool, args) key recurring with the SAME result across other
// tool calls - re-reading the exact same resource late in a long
// incremental-planning session is a legitimate no-op, not a loop. Only the
// (unaffected) adjacency check still applies to it.
func TestRepeatGuardCrossCallScopedToPlanTools(t *testing.T) {
	calls := 0
	states := newRepeatStates()
	g, _ := newRepeatGuard(newNamedRepeatTestTool(t, "read_artifact", &calls), states, nil)
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same-id"}

	other := 0
	og, _ := newRepeatGuard(newNamedRepeatTestTool(t, "edit_plan", &other), states, nil)
	rog := og.(*repeatGuard)

	for i := 1; i <= 5; i++ {
		if _, err := rg.Run(ctx, args); err != nil {
			t.Fatalf("read_artifact call %d: want it to run (not a plan tool, never cross-refused), got %v", i, err)
		}
		if _, err := rog.Run(ctx, map[string]any{"q": fmt.Sprintf("edit %d", i)}); err != nil {
			t.Fatalf("interleaved edit_plan call %d: %v", i, err)
		}
	}
	if calls != 5 {
		t.Fatalf("tool executed %d times; want 5 (never refused)", calls)
	}
}

// TestRepeatGuardCrossCallHardStopAfterIgnoredRefusal is the cross-call
// tier's own hard-stop, mirroring the adjacency one: a model stuck
// alternating two plan tools with the SAME args+result on one of them, that
// keeps going even after being refused, must still end the turn eventually
// - not be refused forever.
func TestRepeatGuardCrossCallHardStopAfterIgnoredRefusal(t *testing.T) {
	calls := 0
	states := newRepeatStates()
	g, _ := newRepeatGuard(newNamedRepeatTestTool(t, "execute", &calls), states, nil)
	rg := g.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same"}

	other := 0
	og, _ := newRepeatGuard(newNamedRepeatTestTool(t, "edit_plan", &other), states, nil)
	rog := og.(*repeatGuard)

	interleave := func(i int) {
		if _, err := rog.Run(ctx, map[string]any{"q": fmt.Sprintf("edit %d", i)}); err != nil {
			t.Fatalf("interleaved edit_plan call %d: %v", i, err)
		}
	}

	// Two real runs to build the cross streak, then keep re-issuing through
	// the soft-refusal tier (repeatHardStopAfter more identical attempts).
	for i := 1; i <= repeatThreshold+repeatHardStopAfter; i++ {
		rg.Run(ctx, args)
		interleave(i)
		if i < repeatThreshold+repeatHardStopAfter && ctx.actions.SkipSummarization {
			t.Fatalf("call %d: SkipSummarization set before the hard-stop tier", i)
		}
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "tool-call loop") {
		t.Fatalf("call after repeated cross-call refusals: want the hard-stop error, got %v", err)
	}
	if !ctx.actions.SkipSummarization {
		t.Error("SkipSummarization = false after the cross-call hard-stop tier, want true")
	}
	if calls != repeatThreshold-1 {
		t.Fatalf("execute ran %d times; want %d (refusals must not execute)", calls, repeatThreshold-1)
	}
}

// TestRepeatGuardVaryingResultNeverRefuses pins the other half of the rig
// regression: execute() was called with identical args every round -
// interleaved with edit_plan calls, exactly the production shape, so
// adjacency never sees two execute() calls back-to-back either - but its
// own result (the judge's rejection reason) varied each time. Genuine
// incremental progress, not a loop, and must never be refused no matter how
// many rounds it takes.
func TestRepeatGuardVaryingResultNeverRefuses(t *testing.T) {
	round := 0
	tl, err := functiontool.New[echoArgs, echoResult](
		functiontool.Config{Name: "execute", Description: "execute"},
		func(_ adkagent.Context, a echoArgs) (echoResult, error) {
			round++
			return echoResult{Out: fmt.Sprintf("rejected: round %d", round)}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	states := newRepeatStates()
	g, err := newRepeatGuard(tl.(runnableTool), states, nil)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	other := 0
	og, _ := newRepeatGuard(newNamedRepeatTestTool(t, "edit_plan", &other), states, nil)
	rog := og.(*repeatGuard)
	ctx := newRepeatCtx("s1")
	args := map[string]any{"q": "same plan every time"}

	for i := 1; i <= 10; i++ {
		if _, err := rg.Run(ctx, args); err != nil {
			t.Fatalf("execute call %d: want it to run (result varies each round), got %v", i, err)
		}
		if _, err := rog.Run(ctx, map[string]any{"q": fmt.Sprintf("no-op edit %d", i)}); err != nil {
			t.Fatalf("interleaved edit_plan call %d: %v", i, err)
		}
	}
	if round != 10 {
		t.Fatalf("tool executed %d times; want 10 (never refused)", round)
	}
}

// A refusal alone doesn't stop a model that ignores it: after
// repeatHardStopAfter more identical calls, the guard ends the node's round
// (via tripped, reaching dag.Executor.RepeatGuardTripped) instead of
// refusing forever, and the streak resets for a subsequent retry.
func TestRepeatGuardEndsRoundAfterRefusalIgnored(t *testing.T) {
	calls := 0
	var gotChat, gotNode, gotMsg string
	tripped := func(chatID, nodeID, msg string) bool {
		gotChat, gotNode, gotMsg = chatID, nodeID, msg
		return true
	}
	g, err := newRepeatGuard(newRepeatTestTool(t, &calls), newRepeatStates(), tripped)
	if err != nil {
		t.Fatal(err)
	}
	rg := g.(*repeatGuard)
	ctx := newRepeatCtxWithAdvisorThread(t, "s1", "chat-1", "node-1")
	args := map[string]any{"q": "same"}

	for i := 1; i <= repeatThreshold+repeatHardStopAfter; i++ {
		if _, err := rg.Run(ctx, args); err == nil && i >= repeatThreshold {
			t.Fatalf("call %d: want REFUSED, got success", i)
		}
	}
	if gotMsg != "" {
		t.Fatalf("tripped fired before the model ignored the refusal %d times: %q", repeatHardStopAfter, gotMsg)
	}
	if _, err := rg.Run(ctx, args); err == nil || !strings.Contains(err.Error(), "tool-call loop") {
		t.Fatalf("call after %d refusals: want the hard-stop error, got %v", repeatThreshold+repeatHardStopAfter, err)
	}
	if gotChat != "chat-1" || gotNode != "node-1" || !strings.Contains(gotMsg, "echo") {
		t.Fatalf("tripped(%q, %q, %q); want chat-1/node-1 and the tool name", gotChat, gotNode, gotMsg)
	}
	if calls != repeatThreshold-1 {
		t.Fatalf("tool executed %d times; want %d (refusals must not execute)", calls, repeatThreshold-1)
	}

	// The streak reset: the same call now runs again instead of re-tripping.
	if _, err := rg.Run(ctx, args); err != nil {
		t.Fatalf("call after hard stop: want a fresh budget, got %v", err)
	}
}
