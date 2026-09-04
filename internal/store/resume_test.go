package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

func resumeTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := st.SetChatOrigin(ctx, "c1", "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	return st
}

// TestSaveDagPlan_DuplicateIsSkippedNotAnError pins #997: a resumed boot
// re-saves the same planID, which used to error on the unique constraint.
func TestSaveDagPlan_DuplicateIsSkippedNotAnError(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan (duplicate re-insert): %v", err)
	}
}

// TestResumePausedDagNodes_HardKillBecomesPausedNotFailed: no shutdown ran,
// so the node is still "running" and owned by this instance. That is a hard
// kill, and a hard kill is resumable state, not a failure.
func TestResumePausedDagNodes_HardKillBecomesPausedNotFailed(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1",
		Status: string(dag.StatusRunning), InstanceID: st.InstanceID()}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	rep, err := st.ResumePausedDagNodes(ctx, nil)
	if err != nil {
		t.Fatalf("ResumePausedDagNodes: %v", err)
	}
	if len(rep.Start) != 1 || rep.Start[0].NodeID != "n1" || rep.Start[0].Reason != dag.PauseShutdown {
		t.Fatalf("Start = %+v, want n1 with reason shutdown", rep.Start)
	}
	got, _ := st.GetDagNode(ctx, "p1", "n1")
	if got.Status != string(dag.StatusPaused) || got.PauseReason != string(dag.PauseShutdown) {
		t.Errorf("node = %q/%q, want paused/shutdown", got.Status, got.PauseReason)
	}
}

// TestResumePausedDagNodes_MissingPlanFails: a node whose plan row is gone
// has nothing to re-enter, so failed is correct - with the reason recorded.
func TestResumePausedDagNodes_MissingPlanFails(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "gone", Status: string(dag.StatusPaused)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	rep, err := st.ResumePausedDagNodes(ctx, nil)
	if err != nil {
		t.Fatalf("ResumePausedDagNodes: %v", err)
	}
	if len(rep.Failed) != 1 || rep.Failed[0].Reason != "plan row is gone" {
		t.Fatalf("Failed = %+v, want one node named with its reason", rep.Failed)
	}
	got, _ := st.GetDagNode(ctx, "gone", "n1")
	if got.Status != string(dag.StatusFailed) || got.Error != "cannot resume: plan row is gone" {
		t.Errorf("node = %q err=%q, want failed with the reason in error", got.Status, got.Error)
	}
}

// TestResumePausedDagNodes_ArchivedChatIsNotResumed pins #1176: a resumable
// callback that rejects archived chats (as serve.go's boot wiring does) must
// mark the node failed with that reason, not hand it back to Start - an
// archived chat's stale paused nodes were being resumed forever in prod.
func TestResumePausedDagNodes_ArchivedChatIsNotResumed(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.ArchiveChat(ctx, "c1", true); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusPaused)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	resumable := func(chatID string) (bool, string) {
		c, err := st.GetChat(ctx, chatID)
		if err == nil && c != nil && c.Archived {
			return false, "chat archived; not resumed"
		}
		return true, ""
	}
	rep, err := st.ResumePausedDagNodes(ctx, resumable)
	if err != nil {
		t.Fatalf("ResumePausedDagNodes: %v", err)
	}
	if len(rep.Start) != 0 {
		t.Fatalf("Start = %+v, want empty - an archived chat's node must not be resumed", rep.Start)
	}
	if len(rep.Failed) != 1 || rep.Failed[0].Reason != "chat archived; not resumed" {
		t.Fatalf("Failed = %+v, want one node with the archived reason", rep.Failed)
	}
	got, _ := st.GetDagNode(ctx, "p1", "n1")
	if got.Status != string(dag.StatusFailed) {
		t.Errorf("node status = %q, want failed", got.Status)
	}
}

// TestScanOrphanedRuns_KeepsPendingQuestion pins #957: the boot scan used to
// blank chats.pending_question, destroying the state a resume needs.
func TestScanOrphanedRuns_KeepsPendingQuestion(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.StampRunOutcome(ctx, "c1", RunStatusNeedsInput, "which region?"); err != nil {
		t.Fatalf("StampRunOutcome: %v", err)
	}
	if err := st.MarkRunActive(ctx, "c1", "t1"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusPaused)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	paused, interrupted, err := st.ScanOrphanedRuns(ctx)
	if err != nil {
		t.Fatalf("ScanOrphanedRuns: %v", err)
	}
	if len(paused) != 1 || len(interrupted) != 0 {
		t.Fatalf("paused=%v interrupted=%v, want the chat counted as paused", paused, interrupted)
	}
	c, err := st.GetChat(ctx, "c1")
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v %v", c, err)
	}
	if c.PendingQuestion != "which region?" {
		t.Errorf("PendingQuestion = %q, want it untouched (#957)", c.PendingQuestion)
	}
	if c.RunStatus != RunStatusPaused {
		t.Errorf("RunStatus = %q, want %q - never interrupted for a chat the server resumes itself", c.RunStatus, RunStatusPaused)
	}
}
