package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

func continuationStore(t *testing.T) *Store {
	t.Helper()
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.db.Create(&Chat{ID: "c1"}).Error; err != nil {
		t.Fatalf("create chat: %v", err)
	}
	return st
}

// TestDagNode_SessionHandleRoundTrip: UpsertDagNode/GetDagNode carry the
// session_handle column through unchanged - the continue primitive's
// durable handle rides on the same node row every other field does.
func TestDagNode_SessionHandleRoundTrip(t *testing.T) {
	st := continuationStore(t)
	ctx := context.Background()
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	handle := `{"kind":"acp","id":"sess-1","agent":"coder","scope":"n1","head_sha":"abc123"}`
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone), SessionHandle: handle}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	got, err := st.GetDagNode(ctx, "p1", "n1")
	if err != nil {
		t.Fatalf("GetDagNode: %v", err)
	}
	if got == nil || got.SessionHandle != handle {
		t.Fatalf("SessionHandle = %+v, want %q", got, handle)
	}
}

// TestFindDagNodeByChat_MostRecentPlanExcludingCurrent: a node id recurs
// across a chat's plans (#... CountDagPlans' own doc), and a follow-up's
// plan is saved before the executor runs - so continue: must resolve
// against the most recent PRIOR plan, not the one currently being built.
func TestFindDagNodeByChat_MostRecentPlanExcludingCurrent(t *testing.T) {
	st := continuationStore(t)
	ctx := context.Background()

	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan p1: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone), SessionHandle: "old"}); err != nil {
		t.Fatalf("UpsertDagNode p1/n1: %v", err)
	}

	if err := st.SaveDagPlan(ctx, "c1", "p2", "t2", "{}"); err != nil {
		t.Fatalf("SaveDagPlan p2: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p2", Status: string(dag.StatusDone), SessionHandle: "newer"}); err != nil {
		t.Fatalf("UpsertDagNode p2/n1: %v", err)
	}

	// p3 is the plan currently being built (already saved, per NewPlanTool's
	// ordering) - it must never be its own continue: target.
	if err := st.SaveDagPlan(ctx, "c1", "p3", "t3", "{}"); err != nil {
		t.Fatalf("SaveDagPlan p3: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p3", Status: string(dag.StatusRunning)}); err != nil {
		t.Fatalf("UpsertDagNode p3/n1: %v", err)
	}

	got, err := st.FindDagNodeByChat(ctx, "c1", "n1", "p3")
	if err != nil {
		t.Fatalf("FindDagNodeByChat: %v", err)
	}
	if got == nil || got.PlanID != "p2" || got.SessionHandle != "newer" {
		t.Fatalf("FindDagNodeByChat = %+v, want plan p2's row", got)
	}
}

func TestFindDagNodeByChat_NoMatch(t *testing.T) {
	st := continuationStore(t)
	ctx := context.Background()
	got, err := st.FindDagNodeByChat(ctx, "c1", "missing", "")
	if err != nil {
		t.Fatalf("FindDagNodeByChat: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil for no match", got)
	}
}

// TestListResumableDagNodes_TerminalWithHandleOnly: the plan tool's
// candidate list is the chat's LATEST plan's terminal nodes that left a
// session handle - a running/paused node or one with no handle isn't a
// candidate.
func TestListResumableDagNodes_TerminalWithHandleOnly(t *testing.T) {
	st := continuationStore(t)
	ctx := context.Background()
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	rows := []DagNode{
		{NodeID: "done-with-handle", PlanID: "p1", Status: string(dag.StatusDone), SessionHandle: `{"agent":"coder"}`, OutputPreview: "did the thing"},
		{NodeID: "done-no-handle", PlanID: "p1", Status: string(dag.StatusDone)},
		{NodeID: "running", PlanID: "p1", Status: string(dag.StatusRunning), SessionHandle: `{"agent":"coder"}`},
		{NodeID: "failed-with-handle", PlanID: "p1", Status: string(dag.StatusFailed), SessionHandle: `{"agent":"coder"}`},
	}
	for _, r := range rows {
		if err := st.UpsertDagNode(ctx, r); err != nil {
			t.Fatalf("UpsertDagNode %s: %v", r.NodeID, err)
		}
	}
	got, err := st.ListResumableDagNodes(ctx, "c1")
	if err != nil {
		t.Fatalf("ListResumableDagNodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.NodeID] = true
	}
	if !ids["done-with-handle"] || !ids["failed-with-handle"] {
		t.Fatalf("candidates = %v, want done-with-handle and failed-with-handle", ids)
	}
}
