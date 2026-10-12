package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
)

// cutChat seeds a run killed mid-plan: plan nodes with deps, rows for the given statuses only (a node that
// never started has no row), and the chat's run still marked in flight.
func cutChat(t *testing.T, nodes []dag.Node, rows map[string]dag.NodeStatus) (*store.Store, string) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx, chatID := context.Background(), "chat-restart"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.SaveDagPlan(ctx, chatID, "plan-1", "turn-1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}
	for id, status := range rows {
		if err := st.UpsertDagNode(ctx, store.DagNode{NodeID: id, PlanID: "plan-1", Status: string(status)}); err != nil {
			t.Fatalf("UpsertDagNode %s: %v", id, err)
		}
	}
	b, _ := json.Marshal(dag.Plan{ID: "plan-1", Nodes: nodes})
	if err := st.SaveExecPlan(ctx, chatID, "plan-1", string(b)); err != nil {
		t.Fatalf("SaveExecPlan: %v", err)
	}
	return st, chatID
}

func startedIDs(rs []store.ResumableNode) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.NodeID)
	}
	slices.Sort(ids)
	return ids
}

// A run killed between nodes resumes from the next one instead of failing: n2 never started, its dependency
// is done, so boot starts it (and its retry runs n3); n3 is covered by n2 and not started on its own.
func TestBootResumesTheNextNodeOfACutRun(t *testing.T) {
	st, chatID := cutChat(t, []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}, {ID: "n3", DependsOn: []string{"n2"}}},
		map[string]dag.NodeStatus{"n1": dag.StatusDone})

	resume := reconcileNodes(context.Background(), st, nil, nil)

	if got := startedIDs(resume.Start); !slices.Equal(got, []string{"n2"}) || len(resume.Settled) != 0 {
		t.Fatalf("Start = %v, Settled = %v; want [n2] and nothing ended", got, resume.Settled)
	}
	if n2 := nodeStatus(t, st, "n2"); n2.Status != string(dag.StatusPaused) || n2.PauseReason != string(dag.PauseShutdown) {
		t.Errorf("n2 = %q/%q, want paused/shutdown so its retry continues it", n2.Status, n2.PauseReason)
	}
	if c, _ := st.GetChat(context.Background(), chatID); c.RunStatus != store.RunStatusPaused {
		t.Errorf("chat run status = %q, want paused, not failed", c.RunStatus)
	}
}

// Two independent branches cut after their shared root: both start, and a node waiting on admission (a
// queued row) is resumed too.
func TestBootResumesEveryBranchOfACutRun(t *testing.T) {
	st, _ := cutChat(t, []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}, {ID: "n3", DependsOn: []string{"n1"}}},
		map[string]dag.NodeStatus{"n1": dag.StatusDone, "n3": dag.StatusQueued})

	resume := reconcileNodes(context.Background(), st, nil, nil)

	if got := startedIDs(resume.Start); !slices.Equal(got, []string{"n2", "n3"}) {
		t.Fatalf("Start = %v, want both branches [n2 n3]", got)
	}
}
