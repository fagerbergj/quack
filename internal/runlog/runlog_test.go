package runlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

// Step must capture the top-level (NodeID == "") agent_complete's model and
// usage - the same signal both rest.Handler.runChat and the SDK extension
// dispatch path (runlog.Drive) rely on to stamp the turn row.
func TestDriveResultStepCapturesTopLevelModelAndUsage(t *testing.T) {
	var res DriveResult
	res.Step(nil, "chat-1", "turn-1", false, stream.SSEEvent{
		Name: stream.EventAgentComplete,
		Data: stream.AgentCompleteData{
			RunID: "orchestrator", Model: "qwen3", PromptTokens: 50, CompletionTokens: 10,
			ReasoningTokens: 5, TotalTokens: 65, CachedTokens: 20,
		},
	})
	if res.Model != "qwen3" {
		t.Fatalf("Model = %q, want qwen3", res.Model)
	}
	want := store.TurnUsage{PromptTokens: 50, CompletionTokens: 10, ReasoningTokens: 5, TotalTokens: 65, CachedTokens: 20}
	if res.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", res.Usage, want)
	}

	// A node-scoped agent_complete (NodeID set) must never be mistaken for
	// the orchestrator's own reply.
	var nodeRes DriveResult
	nodeRes.Step(nil, "chat-1", "turn-1", false, stream.SSEEvent{
		Name: stream.EventAgentComplete,
		Data: stream.AgentCompleteData{NodeID: "n1", RunID: "worker-r0", Model: "qwen3", PromptTokens: 999},
	})
	if nodeRes.Model != "" {
		t.Fatalf("node-scoped agent_complete leaked into DriveResult.Model = %q", nodeRes.Model)
	}
}

// StampTurn is the shared tail (#831's lesson applied to model/usage, not
// just the drain loop): it must write the turn row for a plain-reply turn
// and skip a DAG turn outright (DagNode carries those tokens per-node).
func TestStampTurn(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}

	StampTurn(ctx, st, c.ID, "t1", DriveResult{
		Model: "qwen3", Usage: store.TurnUsage{PromptTokens: 50, CachedTokens: 20},
	})
	turns, err := st.GetTurnsWithContent(ctx, "quack", store.SessionUserFor(*c), c.ID)
	if err != nil || len(turns) != 1 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v", turns, err)
	}
	if turns[0].Model != "qwen3" || turns[0].PromptTokens != 50 || turns[0].CachedTokens != 20 {
		t.Fatalf("turn not stamped: %+v", turns[0])
	}

	// A DAG turn (PlanID set) must not touch the turn row - its tokens live
	// on DagNode instead.
	if err := st.SaveTurn(ctx, c.ID, "t2"); err != nil {
		t.Fatalf("SaveTurn t2: %v", err)
	}
	StampTurn(ctx, st, c.ID, "t2", DriveResult{Model: "qwen3", PlanID: "p1"})
	turns, err = st.GetTurnsWithContent(ctx, "quack", store.SessionUserFor(*c), c.ID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v", turns, err)
	}
	if turns[1].Model != "" {
		t.Fatalf("DAG turn got stamped with a model: %+v", turns[1])
	}
}

// Pins that PersistNodeEvent copies EVERY token field off NodeDoneData -
// CachedTokens was silently dropped once when the struct grew (caught in
// review of the usage-visibility PR); this fails the next time a field is
// added to one side only.
func TestPersistNodeEventCopiesAllTokenFields(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveDagPlan(ctx, c.ID, "p1", "turn-1", `{"plan_id":"p1"}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	PersistNodeEvent(st, "p1", stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{
		NodeID: "n1", Model: "m", PromptTokens: 100, CompletionTokens: 40,
		ReasoningTokens: 8, TotalTokens: 148, CachedTokens: 60, FinishReason: "stop",
	}})
	// PersistNodeEvent writes on its own goroutine (#827) - poll for the
	// "done" status rather than mere row-existence (mirrors
	// rest.waitForDagNodeStatus): UpsertDagNode saves the whole row in one
	// call, so status=="done" and the token fields land atomically together.
	var n *store.DagNode
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := st.GetDagNode(ctx, "p1", "n1")
		if err == nil && got != nil && got.Status == "done" {
			n = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetDagNode: %+v err=%v, node never reached status \"done\"", got, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n.PromptTokens != 100 || n.CompletionTokens != 40 || n.ReasoningTokens != 8 ||
		n.TotalTokens != 148 || n.CachedTokens != 60 || n.Model != "m" {
		t.Errorf("persisted node = %+v, want all token fields copied (cached=60)", n)
	}
}
