package orchestrator

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/session"
)

// TestResetSession_InvokesNodeSessionReaper is a regression test for the ADK
// audit's A2 finding: ResetSession used to delete only the chat's own
// AppName="quack" session, leaving every DAG node's own worker session
// (AppName is whichever agent bundle ran the node) untouched. It must now
// also call the wired node-session reaper (store.ReapNodeSessions in
// production) with the same chat id.
func TestResetSession_InvokesNodeSessionReaper(t *testing.T) {
	sessions := session.InMemoryService()
	o := New(sessions, nil, "", nil, nil, nil, nil, nil)

	var gotCtx context.Context
	var gotChatID string
	calls := 0
	o.SetNodeSessionReaper(func(ctx context.Context, chatID string) error {
		calls++
		gotCtx, gotChatID = ctx, chatID
		return nil
	})

	if err := o.ResetSession(context.Background(), "local", "chat-99"); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	if calls != 1 {
		t.Fatalf("node session reaper called %d times, want 1", calls)
	}
	if gotChatID != "chat-99" {
		t.Fatalf("reaper chat id = %q, want %q", gotChatID, "chat-99")
	}
	if gotCtx == nil {
		t.Fatal("reaper called with a nil context")
	}
}

// TestResetSession_NilReaperIsNoOp confirms the zero Orchestrator (no
// SetNodeSessionReaper call, e.g. in tests that construct one directly)
// still resets the chat-level session without panicking.
func TestResetSession_NilReaperIsNoOp(t *testing.T) {
	sessions := session.InMemoryService()
	o := New(sessions, nil, "", nil, nil, nil, nil, nil)
	if err := o.ResetSession(context.Background(), "local", "chat-1"); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
}
