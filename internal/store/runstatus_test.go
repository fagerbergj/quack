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

	_, ids, err := st.ScanOrphanedRuns(ctx)
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

	_, ids, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}
	if len(ids) != 1 || ids[0] != "chat-interrupted" {
		t.Fatalf("ids = %v, want [chat-interrupted]", ids)
	}
}

// TestScanOrphanedRuns_ClearsInterruptedWithLeftoverTurnID is #920's compound
// state: a chat already stamped interrupted (a previous shutdown's drain) that
// ALSO still carries an ActiveTurnID - the shape a process killed before its
// stamp landed leaves behind. Both halves must settle in one scan, because a
// leftover ActiveTurnID is what makes the chat read as permanently busy;
// recovering it by hand with an UPDATE is not a recovery path.
func TestScanOrphanedRuns_ClearsInterruptedWithLeftoverTurnID(t *testing.T) {
	st := newRunStatusTestStore(t)
	ctx := context.Background()
	if err := st.SetChatOrigin(ctx, "chat-wedged", "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.StampRunOutcome(ctx, "chat-wedged", RunStatusInterrupted, ""); err != nil {
		t.Fatalf("StampRunOutcome: %v", err)
	}
	if err := st.MarkRunActive(ctx, "chat-wedged", "turn-abandoned"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}

	if _, _, err := st.ScanOrphanedRuns(ctx); err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}

	c, err := st.GetChat(ctx, "chat-wedged")
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.ActiveTurnID != "" {
		t.Fatalf("ActiveTurnID = %q, want cleared - the chat is still wedged after boot", c.ActiveTurnID)
	}
	if c.RunStatus != RunStatusInterrupted {
		t.Errorf("RunStatus = %q, want %q", c.RunStatus, RunStatusInterrupted)
	}
}

// TestDeriveTerminalStatus_FailedNodeCarriesItsErrorText is #1105's core
// contract: a failed node's own error string rides along on the derived
// status, so a run that died on a repeated gateway error can be reported as
// such instead of collapsing into the generic silent-gap message.
func TestDeriveTerminalStatus_FailedNodeCarriesItsErrorText(t *testing.T) {
	turns := []TurnContent{{
		AsstText: "",
		Nodes: []DagNode{
			{NodeID: "n1", Status: "done"},
			{NodeID: "n2", Status: "failed", Error: "model gateway failed 5 consecutive attempts over 48m0s: 502 Bad Gateway"},
		},
	}}
	status, question, nodeError := DeriveTerminalStatus(turns, "", false)
	if status != RunStatusFailed {
		t.Fatalf("status = %q, want %q", status, RunStatusFailed)
	}
	if question != "" {
		t.Errorf("question = %q, want empty", question)
	}
	if nodeError != "model gateway failed 5 consecutive attempts over 48m0s: 502 Bad Gateway" {
		t.Errorf("nodeError = %q, want the failed node's own Error text", nodeError)
	}
}

// TestDeriveTerminalStatus_TrueSilentGapStaysUntouched is the negative case
// (#568): an empty answer with no failed node must still report idle with no
// error text - a run that legitimately had nothing to say.
func TestDeriveTerminalStatus_TrueSilentGapStaysUntouched(t *testing.T) {
	turns := []TurnContent{{AsstText: "", Nodes: []DagNode{{NodeID: "n1", Status: "done"}}}}
	status, _, nodeError := DeriveTerminalStatus(turns, "", false)
	if status != RunStatusIdle || nodeError != "" {
		t.Fatalf("status/nodeError = %q/%q, want idle/\"\" for a true silent gap", status, nodeError)
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

	_, ids, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}
