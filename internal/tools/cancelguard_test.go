package tools

import (
	"strings"
	"testing"
)

// newCancelGuarded wraps a recording fake tool in the cancel guard.
func newCancelGuarded(t *testing.T, cancelled map[string]bool) (*fakeRunnable, *cancelGuard) {
	t.Helper()
	inner := &fakeRunnable{}
	g, err := newCancelGuard(inner, func(chatID, nodeID string) bool { return cancelled[chatID+"/"+nodeID] })
	if err != nil {
		t.Fatalf("newCancelGuard: %v", err)
	}
	return inner, g.(*cancelGuard)
}

// TestCancelledNodeToolCallFailsFast is the live bug (2026-07-13): a worker deep
// in a tool loop never reaches a gate-stage boundary, so a user cancel was
// indistinguishable from a no-op for many minutes ("cancel and steer is seemingly
// doing nothing"). The gate check stays the backstop; the TOOL layer is what makes
// a cancelled node stop within ONE tool call.
func TestCancelledNodeToolCallFailsFast(t *testing.T) {
	cancelled := map[string]bool{}
	inner, g := newCancelGuarded(t, cancelled)
	paperclip := newGatedCtx(t, "plan-1", "paperclip", "chat-1")

	// Hot path: a node nobody cancelled is completely unaffected.
	if _, err := g.Run(paperclip, map[string]any{}); err != nil {
		t.Fatalf("uncancelled node: tool call failed: %v", err)
	}
	if inner.runCount() != 1 {
		t.Fatalf("uncancelled node: inner tool ran %d times, want 1", inner.runCount())
	}

	cancelled["chat-1/paperclip"] = true

	_, err := g.Run(paperclip, map[string]any{})
	if err == nil {
		t.Fatal("cancelled node: tool ran anyway; want a stop-now error")
	}
	if !strings.Contains(err.Error(), "CANCELLED") {
		t.Errorf("cancelled node: error %q must tell the model, unmistakably, that the node was CANCELLED", err)
	}
	if inner.runCount() != 1 {
		t.Errorf("cancelled node: the tool EXECUTED (%d runs) - the guard must refuse before running it", inner.runCount())
	}

	// A CONCURRENT sibling node of the same chat/plan keeps working: cancel is
	// per node, not per chat (continue-but-warn).
	stapler := newGatedCtx(t, "plan-1", "stapler", "chat-1")
	if _, err := g.Run(stapler, map[string]any{}); err != nil {
		t.Errorf("sibling node: tool call failed: %v", err)
	}
	if inner.runCount() != 2 {
		t.Errorf("sibling node: inner ran %d times, want 2 (its call must go through)", inner.runCount())
	}
}

// TestCancelGuardIgnoresUngatedCalls: a call with no advisor-thread marker (a
// direct/un-gated invocation, an MCP call, the judge's own read tools) can't be
// attributed to a node, so the guard must never block it - even with a predicate
// that says "cancelled" to everything.
func TestCancelGuardIgnoresUngatedCalls(t *testing.T) {
	inner := &fakeRunnable{}
	g, err := newCancelGuard(inner, func(string, string) bool { return true })
	if err != nil {
		t.Fatalf("newCancelGuard: %v", err)
	}
	if _, err := g.(*cancelGuard).Run(newFakeCtx(), map[string]any{}); err != nil {
		t.Fatalf("un-gated call was blocked: %v", err)
	}
	if inner.runCount() != 1 {
		t.Errorf("un-gated call did not execute (%d runs)", inner.runCount())
	}
}

// TestBuildWrapsEveryToolInTheCancelGuard: the guard is applied at REGISTRATION,
// once, to every tool a worker holds - not sprinkled through the handlers, where
// the next tool added would silently miss it. Without Deps.NodeCancelled (an
// un-gated build, e.g. the judge's read tools) nothing is wrapped.
func TestBuildWrapsEveryToolInTheCancelGuard(t *testing.T) {
	names := []string{"current_date", "ask_user"}

	guarded, err := Build(names, Deps{NodeCancelled: func(string, string) bool { return false }})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for i, tl := range guarded {
		// emitWrap is now the true outermost layer (registry.go's Build) - unwrap
		// it before checking for the cancel guard underneath.
		et, ok := tl.(*emitTool)
		if !ok {
			t.Fatalf("tool %q is not emit-wrapped", names[i])
		}
		cg, ok := et.inner.(*cancelGuard)
		if !ok {
			t.Fatalf("tool %q is not cancel-guarded", names[i])
		}
		if cg.Name() != names[i] || cg.Declaration() == nil {
			t.Errorf("wrapper changed tool %q's identity: name=%q decl=%v", names[i], cg.Name(), cg.Declaration())
		}
	}

	plain, err := Build(names, Deps{})
	if err != nil {
		t.Fatalf("Build (no predicate): %v", err)
	}
	for i, tl := range plain {
		et, ok := tl.(*emitTool)
		if !ok {
			t.Fatalf("tool %q is not emit-wrapped", names[i])
		}
		if _, ok := et.inner.(*cancelGuard); ok {
			t.Errorf("tool %q was wrapped without a NodeCancelled predicate", names[i])
		}
	}
}
