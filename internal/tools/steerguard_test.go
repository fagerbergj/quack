package tools

import (
	"strings"
	"testing"
)

// newSteerGuarded wraps a recording fake tool in the steer guard.
func newSteerGuarded(t *testing.T, guidance map[string]string) (*fakeRunnable, *steerGuard) {
	t.Helper()
	inner := &fakeRunnable{}
	g, err := newSteerGuard(inner, func(chatID, nodeID string) string { return guidance[chatID+"/"+nodeID] })
	if err != nil {
		t.Fatalf("newSteerGuard: %v", err)
	}
	return inner, g.(*steerGuard)
}

// TestSteeredNodeToolCallGetsGuidanceInsteadOfRunning is the tool-layer half of
// steer, sibling to TestCancelledNodeToolCallFailsFast: a steered node's NEXT
// tool call must carry the guidance back to the model as that call's RESULT —
// not run the real tool — so the model hears the redirect within one call
// instead of only after its whole draft finishes at the gate's next stage
// boundary.
func TestSteeredNodeToolCallGetsGuidanceInsteadOfRunning(t *testing.T) {
	guidance := map[string]string{}
	inner, g := newSteerGuarded(t, guidance)
	paperclip := newGatedCtx(t, "plan-1", "paperclip", "chat-1")

	// Hot path: a node with no pending steer is completely unaffected.
	if _, err := g.Run(paperclip, map[string]any{}); err != nil {
		t.Fatalf("unsteered node: tool call failed: %v", err)
	}
	if inner.runCount() != 1 {
		t.Fatalf("unsteered node: inner tool ran %d times, want 1", inner.runCount())
	}

	guidance["chat-1/paperclip"] = "focus on cost"

	res, err := g.Run(paperclip, map[string]any{})
	if err != nil {
		t.Fatalf("steered node: unexpected error: %v", err)
	}
	if inner.runCount() != 1 {
		t.Errorf("steered node: the tool EXECUTED (%d runs) — the guard must intercept before running it", inner.runCount())
	}
	text, _ := res["result"].(string)
	if !strings.Contains(text, "focus on cost") {
		t.Errorf("steered node: result %q must carry the guidance", res)
	}

	// A CONCURRENT sibling node of the same chat/plan is unaffected: steer is
	// per node.
	stapler := newGatedCtx(t, "plan-1", "stapler", "chat-1")
	if _, err := g.Run(stapler, map[string]any{}); err != nil {
		t.Errorf("sibling node: tool call failed: %v", err)
	}
	if inner.runCount() != 2 {
		t.Errorf("sibling node: inner ran %d times, want 2 (its call must go through)", inner.runCount())
	}
}

// TestSteerGuardIgnoresUngatedCalls: a call with no advisor-thread marker can't
// be attributed to a node, so the guard must never intercept it.
func TestSteerGuardIgnoresUngatedCalls(t *testing.T) {
	inner := &fakeRunnable{}
	g, err := newSteerGuard(inner, func(string, string) string { return "should never be seen" })
	if err != nil {
		t.Fatalf("newSteerGuard: %v", err)
	}
	if _, err := g.(*steerGuard).Run(newFakeCtx(), map[string]any{}); err != nil {
		t.Fatalf("un-gated call was blocked: %v", err)
	}
	if inner.runCount() != 1 {
		t.Errorf("un-gated call did not execute (%d runs)", inner.runCount())
	}
}

// TestBuildWrapsEveryToolInTheSteerGuard mirrors
// TestBuildWrapsEveryToolInTheCancelGuard: applied at registration to every
// tool, and cancel stays outermost (a cancelled node's call never reaches the
// steer check).
func TestBuildWrapsEveryToolInTheSteerGuard(t *testing.T) {
	names := []string{"current_date", "ask_user"}

	guarded, err := Build(names, Deps{NodeSteerGuidance: func(string, string) string { return "" }})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for i, tl := range guarded {
		sg, ok := tl.(*steerGuard)
		if !ok {
			t.Fatalf("tool %q is not steer-guarded", names[i])
		}
		if sg.Name() != names[i] || sg.Declaration() == nil {
			t.Errorf("wrapper changed tool %q's identity: name=%q decl=%v", names[i], sg.Name(), sg.Declaration())
		}
	}

	plain, err := Build(names, Deps{})
	if err != nil {
		t.Fatalf("Build (no predicate): %v", err)
	}
	for i, tl := range plain {
		if _, ok := tl.(*steerGuard); ok {
			t.Errorf("tool %q was wrapped without a NodeSteerGuidance predicate", names[i])
		}
	}
}

// TestCancelGuardStaysOutermostOverSteerGuard: when both cancel and steer
// predicates are set, cancel must win — a cancelled node's tool call is
// refused before the steer guard is ever consulted.
func TestCancelGuardStaysOutermostOverSteerGuard(t *testing.T) {
	names := []string{"current_date"}
	guarded, err := Build(names, Deps{
		NodeCancelled:     func(string, string) bool { return false },
		NodeSteerGuidance: func(string, string) string { return "" },
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, ok := guarded[0].(*cancelGuard); !ok {
		t.Fatalf("tool %q's outer wrapper is not the cancel guard: %T", names[0], guarded[0])
	}
}
