package orchestrator

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/session"
)

// ResetSession must also call the node-session reaper with the chat id, since node sessions
// live under each agent bundle's AppName.
func TestResetSession_InvokesNodeSessionReaper(t *testing.T) {
	sessions := session.InMemoryService()
	o := New(sessions, nil, func(context.Context) string { return "" }, nil, nil, nil, nil, nil)

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

// With no reaper wired, ResetSession still resets the chat session.
func TestResetSession_NilReaperIsNoOp(t *testing.T) {
	sessions := session.InMemoryService()
	o := New(sessions, nil, func(context.Context) string { return "" }, nil, nil, nil, nil, nil)
	if err := o.ResetSession(context.Background(), "local", "chat-1"); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
}
