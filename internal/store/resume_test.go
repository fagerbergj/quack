package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
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

// A resumed boot re-saves the same planID; the unique constraint must not turn that into an error.
func TestSaveDagPlan_DuplicateIsSkippedNotAnError(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan (duplicate re-insert): %v", err)
	}
}

// No shutdown ran, so the node is still "running" and owned by this instance: a hard kill, which is
// resumable state, not a failure.
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

// A resumable callback rejecting archived chats (as serve's boot wiring does) marks the node failed with
// that reason instead of resuming it.
func TestResumePausedDagNodes_ArchivedChatIsNotResumed(t *testing.T) {
	st := resumeTestStore(t)
	ctx := context.Background()
	if err := st.ArchiveChat(ctx, "c1", true); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusPaused)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	resumable := func(chatID, pauseReason string) (bool, string) {
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

// The boot scan must not blank chats.pending_question: a resume needs it.
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

	paused, interrupted, _, err := st.ScanOrphanedRuns(ctx)
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

// TestExecPlan_OneRowPerChat: a chat keeps only its latest plan's copy (the only one ever
// resumed), and deleting the chat drops it.
func TestExecPlan_OneRowPerChat(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p1", "p2"} {
		if err := st.SaveExecPlan(ctx, c.ID, id, `{"id":"`+id+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := st.LoadExecPlan(ctx, "p1"); ok {
		t.Error("superseded plan p1 still loads")
	}
	if p, ok := st.LoadExecPlan(ctx, "p2"); !ok || p.ID != "p2" {
		t.Errorf("LoadExecPlan(p2) = %+v, %v; want p2", p, ok)
	}
	if err := st.DeleteChat(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.LoadExecPlan(ctx, "p2"); ok {
		t.Error("exec plan survived DeleteChat")
	}
}

// TestFailUnresumable_AppendsNodeFailed: a boot settle records node.failed in its turn.
func TestFailUnresumable_AppendsNodeFailed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDagPlan(ctx, c.ID, "p1", "turn-1", "{}"); err != nil {
		t.Fatal(err)
	}
	led := ledgertest.NewMemStore()
	st.SetWALLedger(led)
	st.FailUnresumable(ctx, c.ID, DagNode{PlanID: "p1", NodeID: "n1"}, "chat archived")
	entries, _ := led.ReadEntries(ctx, c.ID, 0)
	if len(entries) != 1 || entries[0].Kind != ledger.KindNodeFailed || entries[0].NodeID != "n1" || entries[0].TurnID != "turn-1" {
		t.Fatalf("ledger = %+v, want one node.failed for n1 in turn-1", entries)
	}
}

// A run cut between nodes is resumed from its next node only by the instance that owns it, never a live peer's.
func TestResumePausedDagNodes_QueuedFrontierOwnedOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		owner string
		want  int
	}{{"", 1}, {"live-peer", 0}} {
		st := resumeTestStore(t)
		if err := st.MarkRunActive(ctx, "c1", "t1"); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveExecPlan(ctx, "c1", "p1", `{"ID":"p1","Nodes":[{"ID":"n1"},{"ID":"n2","DependsOn":["n1"]},{"ID":"n3","DependsOn":["n2"]}]}`); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone), InstanceID: tc.owner}); err != nil {
			t.Fatal(err)
		}
		rep, err := st.ResumePausedDagNodes(ctx, nil)
		if err != nil || len(rep.Start) != tc.want {
			t.Errorf("owner %q: Start = %+v (%v), want %d", tc.owner, rep.Start, err, tc.want)
		}
	}
}

// A plan whose rows are split between this instance and a live peer is the peer's run: arming its frontier here
// would run the plan twice.
func TestResumePausedDagNodes_QueuedFrontierSkipsAMixedOwnerPlan(t *testing.T) {
	ctx := context.Background()
	st := resumeTestStore(t)
	if err := st.MarkRunActive(ctx, "c1", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveExecPlan(ctx, "c1", "p1", `{"ID":"p1","Nodes":[{"ID":"n1"},{"ID":"n2"},{"ID":"n3","DependsOn":["n1"]}]}`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []DagNode{
		{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone)},
		{NodeID: "n2", PlanID: "p1", Status: string(dag.StatusRunning), InstanceID: "live-peer"},
	} {
		if err := st.UpsertDagNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.queuedFrontier(ctx, "c1"); len(got) != 0 {
		t.Errorf("frontier = %+v, want none while a live peer holds part of the run", got)
	}
}

// A queued row moves to paused only from queued: a row that moved on underneath the boot scan is left alone.
func TestArmQueued_LeavesARowThatIsNoLongerQueued(t *testing.T) {
	ctx := context.Background()
	st := resumeTestStore(t)
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone)}); err != nil {
		t.Fatal(err)
	}
	if err := st.armQueued(ctx, DagNode{NodeID: "n1", PlanID: "p1"}); err == nil {
		t.Error("armQueued re-stamped a done row paused")
	}
	if n, _ := st.GetDagNode(ctx, "p1", "n1"); n == nil || n.Status != string(dag.StatusDone) {
		t.Errorf("row = %+v, want it left done", n)
	}
}

// A cut run whose plan nodes are all done is left for boot to finish. A failed node, a newer turn cut before it
// planned, or a row a live peer holds still settles failed (or is left alone) instead.
func TestScanOrphanedRuns_FinishableOnlyWhenEveryNodeIsDone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		n2         dag.NodeStatus
		activeTurn string
		n2Owner    string
		finishable bool
	}{
		{"all done", dag.StatusDone, "t1", "", true},
		{"a failed node", dag.StatusFailed, "t1", "", false},
		{"a newer planless turn", dag.StatusDone, "t2", "", false},
		{"a live peer's row", dag.StatusDone, "t1", "live-peer", false},
	} {
		st := resumeTestStore(t)
		if err := st.MarkRunActive(ctx, "c1", tc.activeTurn); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveExecPlan(ctx, "c1", "p1", `{"ID":"p1","Nodes":[{"ID":"n1"},{"ID":"n2"}]}`); err != nil {
			t.Fatal(err)
		}
		for _, n := range []DagNode{
			{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone)},
			{NodeID: "n2", PlanID: "p1", Status: string(tc.n2), InstanceID: tc.n2Owner},
		} {
			if err := st.UpsertDagNode(ctx, n); err != nil {
				t.Fatal(err)
			}
		}
		_, _, finishable, err := st.ScanOrphanedRuns(ctx)
		if err != nil || (len(finishable) == 1) != tc.finishable {
			t.Errorf("%s: finishable = %v (%v), want finishable %v", tc.name, finishable, err, tc.finishable)
		}
	}
}
