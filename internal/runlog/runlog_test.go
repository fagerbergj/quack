package runlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/recordstore"
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
// just the drain loop): it must write the turn row for a plain-reply turn,
// AND for a DAG turn - the orchestrator's own planning tokens are otherwise
// never recorded anywhere, since DagNode only ever carries the workers'.
func TestStampTurn(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveTurn(ctx, c.ID, "t1", ""); err != nil {
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

	// A DAG turn (PlanID set) must ALSO get its own orchestrator tokens
	// stamped - previously a no-op, which made the planning turn's tokens
	// invisible in chat_turns.
	if err := st.SaveTurn(ctx, c.ID, "t2", ""); err != nil {
		t.Fatalf("SaveTurn t2: %v", err)
	}
	StampTurn(ctx, st, c.ID, "t2", DriveResult{
		Model: "qwen3", PlanID: "p1", Usage: store.TurnUsage{PromptTokens: 80055, CachedTokens: 1000},
	})
	turns, err = st.GetTurnsWithContent(ctx, "quack", store.SessionUserFor(*c), c.ID)
	if err != nil || len(turns) != 2 {
		t.Fatalf("GetTurnsWithContent: %+v err=%v", turns, err)
	}
	if turns[1].Model != "qwen3" || turns[1].PromptTokens != 80055 || turns[1].CachedTokens != 1000 {
		t.Fatalf("DAG turn not stamped: %+v", turns[1])
	}

	// GetChatUsage sums ChatTurn and DagNode tokens independently (two
	// separate SUMs) - confirm the DAG turn's stamped tokens land only once,
	// not double-counted through DagNode's own column family.
	agg, err := st.GetChatUsage(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetChatUsage: %v", err)
	}
	if agg.InputTokens != 50+80055 {
		t.Fatalf("GetChatUsage.InputTokens = %d, want %d (t1 + t2, no DagNode rows exist to double-count)", agg.InputTokens, 50+80055)
	}
}

// Pins that PersistNodeEvent copies EVERY token field off NodeDoneData - CachedTokens
// was silently dropped once when the struct grew (caught in review of the
// usage-visibility PR); this fails the next time a field is added to one side only.
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
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{
		NodeID: "n1", Model: "m", PromptTokens: 100, CompletionTokens: 40,
		ReasoningTokens: 8, TotalTokens: 148, CachedTokens: 60, FinishReason: "stop",
	}})
	// PersistNodeEvent writes on its own goroutine (#827) - poll for the "done" status
	// rather than mere row-existence (mirrors rest.waitForDagNodeStatus):
	// UpsertDagNode saves the whole row in one call, so status=="done" and the token fields land atomically together.
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

// TestPersistNodeEvent_FailedAndCancelledCarryContextID is the blocking-
// review regression: a node that ends failed or cancelled - not just done -
// must still get its real transport context id (established before it
// failed) written onto the dag_node record, or a later reuse threads the
// stale mint-time placeholder into session/load instead.
func TestPersistNodeEvent_FailedAndCancelledCarryContextID(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   stream.SSEEvent
	}{
		{"failed", stream.SSEEvent{Name: stream.EventNodeFailed, Data: stream.NodeFailedData{NodeID: "n1", Error: "boom", ContextID: "real-session-failed"}}},
		{"cancelled", stream.SSEEvent{Name: stream.EventNodeCancelled, Data: stream.NodeCancelledData{NodeID: "n1", ContextID: "real-session-cancelled"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dag.SetAgentRoster([]dag.AgentInfo{{Name: "code-implementer"}})
			st := newTestStore(t)
			svc := artifact.InMemoryService()
			st.SetArtifactService(svc)
			ctx := context.Background()
			c, err := st.CreateChat(ctx, "")
			if err != nil {
				t.Fatalf("CreateChat: %v", err)
			}
			if err := st.SaveDagPlan(ctx, c.ID, "p1", "turn-1", `{"plan_id":"p1"}`); err != nil {
				t.Fatalf("SaveDagPlan: %v", err)
			}
			userID := st.SessionUserForChat(ctx, c.ID)
			rc := recordstore.New(svc, artifactref.AppName, userID, c.ID)
			seed := dag.DagNodeRecord{NodeID: "n1", Agent: "code-implementer", Status: dag.StatusRunning, ContextID: "placeholder", Started: true}
			if _, _, err := rc.SaveStructured(ctx, "dag_node", seed, "n1", recordstore.Lineage{}); err != nil {
				t.Fatalf("seed dag_node: %v", err)
			}

			PersistNodeEvent(st, c.ID, "p1", tc.ev)

			var got dag.DagNodeRecord
			deadline := time.Now().Add(2 * time.Second)
			for {
				raw, _, ok, err := rc.Latest(ctx, "dag_node:n1")
				if err == nil && ok {
					if uerr := json.Unmarshal(raw, &got); uerr != nil {
						t.Fatalf("unmarshal dag_node: %v", uerr)
					}
					if got.ContextID != "placeholder" {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("dag_node ContextID never updated off the placeholder (got %+v)", got)
				}
				time.Sleep(5 * time.Millisecond)
			}
			wantID := "real-session-" + tc.name
			if got.ContextID != wantID {
				t.Errorf("dag_node.ContextID = %q, want %q", got.ContextID, wantID)
			}
		})
	}
}

// A genuine iter.Seq2 range loop, not a fake counting yield: proves Drive's recover
// holds against real rangefunc poisoning (#1016), which a plain closure test
// cannot exercise (see orchestrator's TestSafeYieldConcurrent*). Mirrors
// orchestrator.newSafeYield: recovers a real loop-body panic (Drive's own onErr call,
// triggered by a non-nil err event - not a synthetic closure), then keeps calling yield
// exactly like orchestrator.Run does after a recovered node panic during RunPlanAsGraph. That second call either re-panics with "range function continued iteration after loop body panic", or - if it never fires - Drive's own return triggers "range function recovered a loop body panic and did not resume panicking". Both are verified reproducible with a minimal Go 1.23+ program outside this repo; Drive's defer/recover must catch whichever one actually happens here.
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

// waitForNodeStoreStatus polls the runlog store row (not the dag_node
// record) for status - PersistNodeEvent's own "from" comes from here.
func waitForNodeStoreStatus(t *testing.T, st *store.Store, planID, nodeID, want string) *store.DagNode {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := st.GetDagNode(context.Background(), planID, nodeID)
		if err == nil && got != nil && got.Status == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetDagNode: %+v err=%v, node never reached status %q", got, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPersistNodeEventReusedNodeTransitionsThroughQueued is the QA rig
// regression (#slice3 review): "persistNodeEvent: dag_node status update
// failed ... illegal status transition done -> running" for a node reused
// in a later incremental step. dag.CanTransition refuses done -> running
// directly (only done -> queued -> running is legal) - the incremental step
// path was starting a reused node straight into node_start without a
// node_queued first, unlike the fresh-hire path. With that fixed
// (RunPlanStep now queues every node in its run set), the same sequence a
// real reuse produces - node_queued, node_start, node_done - must carry the
// STORE row and the dag_node RECORD through done -> queued -> running ->
// done with no illegal-transition warning logged.
func TestPersistNodeEventReusedNodeTransitionsThroughQueued(t *testing.T) {
	dag.SetAgentRoster([]dag.AgentInfo{{Name: "code-implementer"}})
	st := newTestStore(t)
	svc := artifact.InMemoryService()
	st.SetArtifactService(svc)
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := st.SaveDagPlan(ctx, c.ID, "p1", "turn-1", `{"plan_id":"p1"}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	userID := st.SessionUserForChat(ctx, c.ID)
	rc := recordstore.New(svc, artifactref.AppName, userID, c.ID)
	seed := dag.DagNodeRecord{NodeID: "n1", Agent: "code-implementer", Status: dag.StatusDone, Started: true}
	if _, _, err := rc.SaveStructured(ctx, "dag_node", seed, "n1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node record: %v", err)
	}

	// Step 1: n1's real first run - establishes the STORE row's own "done"
	// too (independent of the record seeded above), through the normal
	// queued -> running -> done sequence a fresh dispatch takes.
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeQueued, Data: stream.NodeQueuedData{NodeID: "n1"}})
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeStart, Data: stream.NodeStartData{NodeID: "n1", Agent: "code-implementer"}})
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{NodeID: "n1"}})
	waitForNodeStoreStatus(t, st, "p1", "n1", "done")

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	// Step 2: n1 reassigned - the fixed incremental step path queues before running.
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeQueued, Data: stream.NodeQueuedData{NodeID: "n1"}})
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeStart, Data: stream.NodeStartData{NodeID: "n1", Agent: "code-implementer"}})
	PersistNodeEvent(st, c.ID, "p1", stream.SSEEvent{Name: stream.EventNodeDone, Data: stream.NodeDoneData{NodeID: "n1"}})

	waitForNodeStoreStatus(t, st, "p1", "n1", "done")
	if strings.Contains(buf.String(), "illegal") {
		t.Errorf("want no illegal-transition warning logged, got: %s", buf.String())
	}

	var got dag.DagNodeRecord
	deadline := time.Now().Add(2 * time.Second)
	for {
		raw, _, ok, rerr := rc.Latest(ctx, "dag_node:n1")
		if rerr == nil && ok {
			if uerr := json.Unmarshal(raw, &got); uerr != nil {
				t.Fatalf("unmarshal dag_node: %v", uerr)
			}
			if got.Status == dag.StatusDone {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dag_node record never settled back to done (got %+v)", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
