package serve

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// storePauser stands in for the live dag.Executor: the same synchronous
// store write nodeControl.markPaused performs, over the real store, without
// booting an LLM-backed executor to get one node mid-round.
type storePauser struct {
	st     *store.Store
	active map[string][]string
}

func (p *storePauser) ActiveNodes(chatID string) []string { return p.active[chatID] }

func (p *storePauser) PauseNode(chatID, nodeID string, reason dag.PauseReason) bool {
	return p.st.SetNodeStatusForChat(context.Background(), chatID, nodeID,
		string(dag.StatusPaused), string(reason), "") == nil
}

// threeNodeChat seeds the acceptance shape from #962: n1 done, n2 mid-round,
// n3 queued, with the chat's run marked in flight.
func threeNodeChat(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	chatID := "chat-restart"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.SaveDagPlan(ctx, chatID, "plan-1", "turn-1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	for nodeID, status := range map[string]dag.NodeStatus{
		"n1": dag.StatusDone, "n2": dag.StatusRunning, "n3": dag.StatusQueued,
	} {
		if err := st.UpsertDagNode(ctx, store.DagNode{NodeID: nodeID, PlanID: "plan-1", Status: string(status)}); err != nil {
			t.Fatalf("UpsertDagNode %s: %v", nodeID, err)
		}
	}
	if err := st.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}
	return st, chatID
}

func nodeStatus(t *testing.T, st *store.Store, nodeID string) store.DagNode {
	t.Helper()
	n, err := st.GetDagNode(context.Background(), "plan-1", nodeID)
	if err != nil || n == nil {
		t.Fatalf("GetDagNode %s: %+v %v", nodeID, n, err)
	}
	return *n
}

// TestShutdownPersistsPausedNodes is the first half of #962's restart
// acceptance: a drain leaves a done node alone, the running node
// paused/shutdown on disk, the queued node queued, and the chat NOT
// interrupted.
func TestShutdownPersistsPausedNodes(t *testing.T) {
	st, chatID := threeNodeChat(t)
	hub := stream.NewHub()
	hub.RegisterRun(chatID, "turn-1", func() {})
	go func() {
		time.Sleep(30 * time.Millisecond)
		hub.UnregisterRun(chatID) // n2 reaches a gate boundary inside the grace window
	}()

	DrainActiveRuns(hub, &storePauser{st: st, active: map[string][]string{chatID: {"n2"}}}, 2*time.Second)

	if n := nodeStatus(t, st, "n1"); n.Status != string(dag.StatusDone) {
		t.Errorf("n1 = %q, want done (a finished node must be untouched)", n.Status)
	}
	n2 := nodeStatus(t, st, "n2")
	if n2.Status != string(dag.StatusPaused) || n2.PauseReason != string(dag.PauseShutdown) {
		t.Errorf("n2 = %q/%q, want paused/shutdown", n2.Status, n2.PauseReason)
	}
	if n := nodeStatus(t, st, "n3"); n.Status != string(dag.StatusQueued) {
		t.Errorf("n3 = %q, want queued", n.Status)
	}
	if hub.WasInterrupted(chatID) {
		t.Error("shutdown marked the chat interrupted; a paused run is resumed by the server itself")
	}
}

// TestBootResumesPausedNodes is the second half: a fresh store handle over
// the same database reconciles that persisted state into a node to start,
// and stamps the chat paused rather than interrupted.
func TestBootResumesPausedNodes(t *testing.T) {
	st, chatID := threeNodeChat(t)
	ctx := context.Background()
	if err := st.SetNodeStatusForChat(ctx, chatID, "n2", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatalf("persist the shutdown pause: %v", err)
	}

	start := reconcileNodes(ctx, st, nil)

	if len(start) != 1 || start[0].NodeID != "n2" || start[0].ChatID != chatID {
		t.Fatalf("resumable nodes = %+v, want just n2 on %s", start, chatID)
	}
	if start[0].Reason != dag.PauseShutdown {
		t.Errorf("resume reason = %q, want shutdown", start[0].Reason)
	}
	if n := nodeStatus(t, st, "n1"); n.Status != string(dag.StatusDone) {
		t.Errorf("n1 = %q, want done", n.Status)
	}
	if n := nodeStatus(t, st, "n3"); n.Status != string(dag.StatusQueued) {
		t.Errorf("n3 = %q, want queued so the resumed graph schedules it", n.Status)
	}
	c, err := st.GetChat(ctx, chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v %v", c, err)
	}
	if c.RunStatus != store.RunStatusPaused {
		t.Errorf("RunStatus = %q, want %q", c.RunStatus, store.RunStatusPaused)
	}
	if c.ActiveTurnID != "" {
		t.Errorf("ActiveTurnID = %q, want cleared", c.ActiveTurnID)
	}
}

// TestBootLeavesAwaitingInputAlone is the HITL variant: a node parked on a
// question keeps its question and is never started - it needs an answer.
func TestBootLeavesAwaitingInputAlone(t *testing.T) {
	st, chatID := threeNodeChat(t)
	ctx := context.Background()
	if err := st.SetNodeStatusForChat(ctx, chatID, "n2", string(dag.StatusPaused),
		string(dag.PauseAwaitingInput), "which region?"); err != nil {
		t.Fatalf("park n2: %v", err)
	}

	start := reconcileNodes(ctx, st, nil)

	if len(start) != 0 {
		t.Errorf("started %+v; a node awaiting input must not be started", start)
	}
	n2 := nodeStatus(t, st, "n2")
	if n2.Status != string(dag.StatusPaused) || n2.PauseReason != string(dag.PauseAwaitingInput) {
		t.Errorf("n2 = %q/%q, want paused/awaiting_input", n2.Status, n2.PauseReason)
	}
	if n2.PendingQuestion != "which region?" {
		t.Errorf("question = %q, want it intact across the restart", n2.PendingQuestion)
	}
}

// TestBootFailsUnresumableNode: a node whose workspace is gone is the one
// remaining path to failed, and the reason lands in `error`.
func TestBootFailsUnresumableNode(t *testing.T) {
	st, chatID := threeNodeChat(t)
	ctx := context.Background()
	if err := st.SetNodeStatusForChat(ctx, chatID, "n2", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatalf("pause n2: %v", err)
	}

	start := reconcileNodes(ctx, st, func(string) (bool, string) { return false, "workspace dir is gone" })

	if len(start) != 0 {
		t.Errorf("started %+v; an unresumable node must not be started", start)
	}
	n2 := nodeStatus(t, st, "n2")
	if n2.Status != string(dag.StatusFailed) {
		t.Errorf("n2 = %q, want failed", n2.Status)
	}
	if n2.Error != "cannot resume: workspace dir is gone" {
		t.Errorf("error = %q, want the reason it could not be resumed", n2.Error)
	}
}
