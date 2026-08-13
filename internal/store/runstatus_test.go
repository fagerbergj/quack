package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newRunStatusTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

// TestScanOrphanedRuns_FlipsCrashedActiveTurnID is #738's crash case: a
// process died with ActiveTurnID set. A boot scan must convert that into a
// clean RunStatusInterrupted stamp, not leave the chat reading as stuck.
func TestScanOrphanedRuns_FlipsCrashedActiveTurnID(t *testing.T) {
	st := newRunStatusTestStore(t)
	ctx := context.Background()
	if err := st.SetChatOrigin(ctx, "chat-crashed", "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.MarkRunActive(ctx, "chat-crashed", "turn-1"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}

	ids, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}
	if len(ids) != 1 || ids[0] != "chat-crashed" {
		t.Fatalf("ids = %v, want [chat-crashed]", ids)
	}

	c, err := st.GetChat(ctx, "chat-crashed")
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.RunStatus != RunStatusInterrupted {
		t.Errorf("RunStatus = %q, want %q", c.RunStatus, RunStatusInterrupted)
	}
	if c.ActiveTurnID != "" {
		t.Errorf("ActiveTurnID = %q, want cleared", c.ActiveTurnID)
	}
}

// TestScanOrphanedRuns_ReSurfacesAlreadyInterrupted proves a chat a previous
// shutdown already marked interrupted is picked up again at the next boot
// too - it stays visible until someone actually resumes it.
func TestScanOrphanedRuns_ReSurfacesAlreadyInterrupted(t *testing.T) {
	st := newRunStatusTestStore(t)
	ctx := context.Background()
	if err := st.SetChatOrigin(ctx, "chat-interrupted", "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.StampRunOutcome(ctx, "chat-interrupted", RunStatusInterrupted, ""); err != nil {
		t.Fatalf("StampRunOutcome: %v", err)
	}

	ids, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}
	if len(ids) != 1 || ids[0] != "chat-interrupted" {
		t.Fatalf("ids = %v, want [chat-interrupted]", ids)
	}
}

// TestScanOrphanedRuns_LeavesHealthyChatsAlone is the negative case: idle,
// failed, and needs_input chats with no ActiveTurnID must never be touched.
func TestScanOrphanedRuns_LeavesHealthyChatsAlone(t *testing.T) {
	st := newRunStatusTestStore(t)
	ctx := context.Background()
	for _, status := range []string{RunStatusIdle, RunStatusFailed, RunStatusNeedsInput} {
		chatID := "chat-" + status
		if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
			t.Fatalf("SetChatOrigin(%s): %v", chatID, err)
		}
		if err := st.StampRunOutcome(ctx, chatID, status, ""); err != nil {
			t.Fatalf("StampRunOutcome(%s): %v", chatID, err)
		}
	}

	ids, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}
