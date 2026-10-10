package tools

import (
	"strings"
	"testing"
)

func newCancelGuarded(t *testing.T, cancelled map[string]bool) (*fakeRunnable, hooked) {
	t.Helper()
	inner := &fakeRunnable{}
	h := NewHooks(Deps{NodeCancelled: func(chatID, nodeID string) bool { return cancelled[chatID+"/"+nodeID] }}, 0)
	return inner, hook(h, HookNode, inner)
}

// A worker deep in a tool loop never reaches a gate-stage boundary, so the tool layer makes a cancelled node
// stop within one tool call.
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

	// A concurrent sibling node of the same chat keeps working: cancel is per node.
	stapler := newGatedCtx(t, "plan-1", "stapler", "chat-1")
	if _, err := g.Run(stapler, map[string]any{}); err != nil {
		t.Errorf("sibling node: tool call failed: %v", err)
	}
	if inner.runCount() != 2 {
		t.Errorf("sibling node: inner ran %d times, want 2 (its call must go through)", inner.runCount())
	}
}

// A call with no node token (ungated, MCP) has no node to attribute, so the guard never blocks it.
func TestCancelGuardIgnoresUngatedCalls(t *testing.T) {
	inner := &fakeRunnable{}
	g := hook(NewHooks(Deps{NodeCancelled: func(string, string) bool { return true }}, 0), HookNode, inner)
	if _, err := g.Run(newFakeCtx(), map[string]any{}); err != nil {
		t.Fatalf("un-gated call was blocked: %v", err)
	}
	if inner.runCount() != 1 {
		t.Errorf("un-gated call did not execute (%d runs)", inner.runCount())
	}
}

// The guard rides every Build tool's hooks, so a new tool cannot miss it. Without
// Deps.NodeCancelled (ungated build) nothing is refused.
func TestBuildWrapsEveryToolInTheCancelGuard(t *testing.T) {
	names := []string{"current_date", "ask_user"}
	ctx := newGatedCtx(t, "plan-1", "paperclip", "chat-1")

	for _, c := range []struct {
		d       Deps
		refused bool
	}{{Deps{NodeCancelled: func(string, string) bool { return true }}, true}, {Deps{}, false}} {
		built, err := Build(names, c.d)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		h := NewHooks(c.d, 0)
		for i, tl := range built {
			if tl.Name() != names[i] {
				t.Errorf("Build changed tool %q's identity: name=%q", names[i], tl.Name())
			}
			_, err := hook(h, HookBuilt, tl).Run(ctx, map[string]any{})
			if got := err != nil && strings.Contains(err.Error(), "CANCELLED"); got != c.refused {
				t.Errorf("tool %q: cancelled refusal = %v (err %v), want %v", names[i], got, err, c.refused)
			}
		}
	}
}
