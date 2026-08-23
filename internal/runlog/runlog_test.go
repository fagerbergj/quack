package runlog

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
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

// A genuine iter.Seq2 range loop, not a fake counting yield: proves Drive's
// recover holds against real rangefunc poisoning (#1016), which a plain
// closure test cannot exercise (see orchestrator's TestSafeYieldConcurrent*).
//
// Mirrors orchestrator.newSafeYield: recovers a real loop-body panic (Drive's
// own onErr call, triggered by a non-nil err event - not a synthetic
// closure), then keeps calling yield exactly like orchestrator.Run does
// after a recovered node panic during RunPlanAsGraph. That second call
// either re-panics with "range function continued iteration after loop body
// panic", or - if it never fires - Drive's own return triggers "range
// function recovered a loop body panic and did not resume panicking". Both
// are verified reproducible with a minimal Go 1.23+ program outside this
// repo; Drive's defer/recover must catch whichever one actually happens here.
func TestDriveRecoversPoisonedRangeState(t *testing.T) {
	const boom = "distinctive-drive-loop-body-panic"
	safeYield := func(yield func(stream.SSEEvent, error) bool) func(stream.SSEEvent, error) bool {
		var mu sync.Mutex
		stopped := false
		return func(ev stream.SSEEvent, err error) (ok bool) {
			mu.Lock()
			defer mu.Unlock()
			if stopped {
				return false
			}
			defer func() {
				if recover() != nil {
					stopped = true
					ok = false
				}
			}()
			return yield(ev, err)
		}
	}

	run := func(yield func(stream.SSEEvent, error) bool) {
		sy := safeYield(yield)
		sy(stream.SSEEvent{}, errors.New("trigger")) // Drive's own onErr(err) panics inside its loop body
		sy(stream.Done(), nil)                       // re-enters the now-poisoned range state
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		Drive("turn-1", nil, nil, run, func(error) { panic(boom) })
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drive never returned - a poisoned rangefunc panic likely killed this goroutine")
	}
}
