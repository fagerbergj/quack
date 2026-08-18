package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

func nodeStateStore(t *testing.T) *Store {
	t.Helper()
	st, err := New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := st.SaveDagPlan(ctx, "c1", "p1", "t1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusRunning)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	return st
}

// TestSetNodeStatus_PersistsPauseAndRejectsIllegal: the one transition
// write-through both stamps pause metadata and refuses done → running.
func TestSetNodeStatus_PersistsPauseAndRejectsIllegal(t *testing.T) {
	st := nodeStateStore(t)
	ctx := context.Background()

	if err := st.SetNodeStatusForChat(ctx, "c1", "n1", string(dag.StatusPaused), string(dag.PauseAwaitingInput), "which region?"); err != nil {
		t.Fatalf("SetNodeStatusForChat: %v", err)
	}
	status, reason, q, _, err := st.GetNodeState(ctx, "c1", "n1")
	if err != nil {
		t.Fatalf("GetNodeState: %v", err)
	}
	if status != string(dag.StatusPaused) || reason != string(dag.PauseAwaitingInput) || q != "which region?" {
		t.Fatalf("got (%q, %q, %q); want (paused, awaiting_input, \"which region?\")", status, reason, q)
	}

	// A plain node_done upsert must not blank the lifecycle columns.
	if err := st.UpsertDagNode(ctx, DagNode{NodeID: "n1", PlanID: "p1", Status: string(dag.StatusDone), Output: "hi"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	if _, _, q, _, _ := st.GetNodeState(ctx, "c1", "n1"); q != "which region?" {
		t.Errorf("pending_question blanked by an unrelated upsert: %q", q)
	}

	if err := st.SetNodeStatus(ctx, "p1", "n1", dag.StatusRunning, "", ""); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("done → running: err = %v; want ErrIllegalTransition", err)
	}
}

// TestListPausedDagNodes is PR 2's boot sweep: every suspended node, both
// spellings, and nothing terminal.
func TestListPausedDagNodes(t *testing.T) {
	st := nodeStateStore(t)
	ctx := context.Background()
	for _, n := range []DagNode{
		{NodeID: "n2", PlanID: "p1", Status: string(dag.StatusNeedsInput)},
		{NodeID: "n3", PlanID: "p1", Status: string(dag.StatusDone)},
	} {
		if err := st.UpsertDagNode(ctx, n); err != nil {
			t.Fatalf("UpsertDagNode: %v", err)
		}
	}
	if err := st.SetNodeStatus(ctx, "p1", "n1", dag.StatusPaused, dag.PauseShutdown, ""); err != nil {
		t.Fatalf("SetNodeStatus: %v", err)
	}
	got, err := st.ListPausedDagNodes(ctx)
	if err != nil {
		t.Fatalf("ListPausedDagNodes: %v", err)
	}
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.NodeID] = true
	}
	if len(got) != 2 || !ids["n1"] || !ids["n2"] {
		t.Errorf("paused nodes = %v; want n1 and n2 only", ids)
	}
}
