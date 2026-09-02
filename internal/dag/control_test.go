package dag

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// coopStub records worker + judge calls and blocks the FIRST worker call until the
// test unblocks it - so the test can set a cancel/steer via the executor before
// the gate reaches its next stage boundary. The judge always passes.
type coopStub struct {
	mu          sync.Mutex
	workerCalls int
	prompts     []string
	judgeCalls  int
	started     chan struct{}
	unblock     chan struct{}
}

func (*coopStub) Name() string { return "coopStub" }

func (s *coopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			s.mu.Lock()
			s.judgeCalls++
			s.mu.Unlock()
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.workerCalls++
		n := s.workerCalls
		s.prompts = append(s.prompts, gUserText(req))
		s.mu.Unlock()
		if n == 1 {
			select {
			case s.started <- struct{}{}:
			default:
			}
			<-s.unblock // let the test inject cancel/steer before the gate boundary
		}
		yield(gText("draft"), nil)
	}
}

func newCoopExecutor(t *testing.T, stub *coopStub, rounds int) (*Executor, Plan) {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: rounds} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}
	return ex, plan
}

func drain(t *testing.T, ex *Executor, plan Plan) {
	t.Helper()
	runPlanSSE(t, ex, plan, "chat")
}

// TestExecute_TaskOverrideAppliesBeforeNodeStarts: SetNodeTaskOverride, called
// before the node has started, actually drives the worker's prompt - a
// regression test for the override having been dead code (getOverride was
// never consulted; the node ran the plan's original node.Task regardless of
// what the REST 200 implied was saved).
func TestExecute_TaskOverrideAppliesBeforeNodeStarts(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	close(stub.unblock) // nothing to synchronize on; the override lands well before drain
	ex, plan := newCoopExecutor(t, stub, 1)

	if !ex.SetNodeTaskOverride("chat", "n1", "REVISED TASK TEXT") {
		t.Fatal("SetNodeTaskOverride returned false for a not-yet-started node")
	}
	drain(t, ex, plan)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.prompts) == 0 {
		t.Fatal("worker never ran")
	}
	if !strings.Contains(stub.prompts[0], "REVISED TASK TEXT") {
		t.Errorf("worker prompt missing the overridden task: %q", stub.prompts[0])
	}
	if strings.Contains(stub.prompts[0], "do it") {
		t.Errorf("worker prompt still contains the plan's ORIGINAL task text: %q", stub.prompts[0])
	}
}

// TestExecute_TaskOverrideRejectedOnceNodeStarted: closes the TOCTOU between
// "is this node started?" and "stash the override" - registerAndTakeOverride
// registers the live control BEFORE the worker's first call, so an override
// attempt that only lands once the worker is already running (as this test
// forces via coopStub's start signal) must be rejected outright, never
// silently accepted-but-ignored.
func TestExecute_TaskOverrideRejectedOnceNodeStarted(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	var accepted bool
	go func() {
		<-stub.started
		accepted = ex.SetNodeTaskOverride("chat", "n1", "too late")
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	if accepted {
		t.Error("SetNodeTaskOverride succeeded after the node had already started")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if strings.Contains(stub.prompts[0], "too late") {
		t.Error("a rejected override still leaked into the running node's prompt")
	}
}

// TestExecute_CancelNodeStopsBeforeJudge: cancelling a running node makes the gate
// stop at its next stage boundary - so the judge never runs.
func TestExecute_CancelNodeStopsBeforeJudge(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	go func() {
		<-stub.started
		ex.CancelNode("chat", "n1") // set cancel while the worker is mid-draft
		close(stub.unblock)         // worker finishes → gate checks cancel before judging
	}()
	drain(t, ex, plan)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.judgeCalls != 0 {
		t.Errorf("judge ran %d times; cancel should have stopped the node before judging", stub.judgeCalls)
	}
}

// judgeBlockStub blocks the FIRST judge call (the one carrying submit_verdict)
// until the test unblocks it, then returns a failing verdict - so the test can
// inject a cancel while the judge's own model call is in flight and confirm the
// gate stops before the revise round starts. The worker draft never blocks.
type judgeBlockStub struct {
	mu          sync.Mutex
	workerCalls int
	judgeCalls  int
	started     chan struct{}
	unblock     chan struct{}
}

func (*judgeBlockStub) Name() string { return "judgeBlockStub" }

func (s *judgeBlockStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			s.mu.Lock()
			s.judgeCalls++
			n := s.judgeCalls
			s.mu.Unlock()
			if n == 1 {
				select {
				case s.started <- struct{}{}:
				default:
				}
				<-s.unblock // let the test inject cancel before the verdict is even known
			}
			yield(gCall("submit_verdict", map[string]any{"score": 0.1, "feedback": "needs work"}), nil)
			return
		}
		s.mu.Lock()
		s.workerCalls++
		s.mu.Unlock()
		yield(gText("draft"), nil)
	}
}

func newJudgeBlockExecutor(t *testing.T, stub *judgeBlockStub, rounds int) (*Executor, Plan) {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: rounds} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}
	return ex, plan
}

// TestExecute_CancelDuringJudgeStopsBeforeRevise: the cooperative ctrl check
// used to run only at the TOP of each judge round, so a cancel that lands
// while the judge's own model call is in flight still paid for a full revise
// round before the next boundary honored it (#879 incident). A cancel set
// during the judge call must stop the gate before revise starts.
func TestExecute_CancelDuringJudgeStopsBeforeRevise(t *testing.T) {
	stub := &judgeBlockStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newJudgeBlockExecutor(t, stub, 1)

	go func() {
		<-stub.started
		ex.CancelNode("chat", "n1") // set cancel while the judge is mid-verdict
		close(stub.unblock)         // judge returns a FAILING verdict → gate must not start revise
	}()
	events, _ := runPlanSSE(t, ex, plan, "chat")

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.workerCalls != 1 {
		t.Errorf("worker ran %d times; a cancel during the judge round should have stopped the gate before the revise round started", stub.workerCalls)
	}
	if got := nodeEnd(events, "n1"); got != stream.EventNodeCancelled {
		t.Errorf("n1 ended as %q; want node_cancelled", got)
	}
}

// nodeEnd returns the terminal event name (node_done / node_failed / node_cancelled)
// for a node.
func nodeEnd(events []stream.SSEEvent, nodeID string) string {
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.NodeDoneData:
			if d.NodeID == nodeID {
				return stream.EventNodeDone
			}
		case stream.NodeFailedData:
			if d.NodeID == nodeID {
				return stream.EventNodeFailed
			}
		case stream.NodeCancelledData:
			if d.NodeID == nodeID {
				return stream.EventNodeCancelled
			}
		case stream.NodePausedData:
			if d.NodeID == nodeID {
				return stream.EventNodePaused
			}
		}
	}
	return ""
}

// TestExecute_CancelFlagDoesNotLeakAcrossTurns: node IDs (n1, n2, …) repeat every
// turn, and the user-cancelled flag survives its control's unregister - so without
// a per-turn reset a node cancelled last turn marks THIS turn's same-ID node
// "stopped". ResetNodeCancels (called at the start of each Run) clears it.
func TestExecute_CancelFlagDoesNotLeakAcrossTurns(t *testing.T) {
	// A prior turn's cancel left cancelled["s"]["n1"] set; this turn n1 completes.
	newRun := func() (*Executor, Plan) {
		stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
		close(stub.unblock) // never block: worker answers, judge passes
		ex, plan := newCoopExecutor(t, stub, 1)
		ex.controls.cancelled = map[string]map[string]bool{"s": {"n1": true}}
		return ex, plan
	}

	t.Run("reset clears it", func(t *testing.T) {
		ex, plan := newRun()
		ex.ResetNodeCancels("s")
		events, _ := runPlanSSE(t, ex, plan, "s")
		if got := nodeEnd(events, "n1"); got != stream.EventNodeDone {
			t.Errorf("n1 ended as %q; want node_done after reset", got)
		}
	})

	// Guard the guard: without the reset the stale flag DOES corrupt this turn, so
	// the reset above is load-bearing rather than a no-op.
	t.Run("without reset it leaks", func(t *testing.T) {
		ex, plan := newRun()
		events, _ := runPlanSSE(t, ex, plan, "s")
		if got := nodeEnd(events, "n1"); got != stream.EventNodeCancelled {
			t.Errorf("n1 ended as %q; want the leak (node_cancelled) that proves reset matters", got)
		}
	})
}

// TestExecute_QueueNodeMessageReRunsWithGuidance: queueing a message for a
// running node re-runs its worker with the message folded in (drained at the
// next gate-stage boundary, never mid-call), then proceeds to the judge.
func TestExecute_QueueNodeMessageReRunsWithGuidance(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	go func() {
		<-stub.started
		if _, ok := ex.QueueNodeMessage("chat", "n1", "focus on cost"); !ok {
			t.Error("QueueNodeMessage returned false for a LIVE node")
		}
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.workerCalls != 2 {
		t.Fatalf("worker ran %d times; a queued message should trigger a second (guided) run", stub.workerCalls)
	}
	if !strings.Contains(stub.prompts[1], "focus on cost") {
		t.Errorf("re-run prompt missing the queued message: %q", stub.prompts[1])
	}
	if stub.judgeCalls != 1 {
		t.Errorf("judge ran %d times; expected 1 after the re-run", stub.judgeCalls)
	}
}

// TestExecute_QueueNodeMessageDeliversLiveVsBoundary (#998): with a live-steer
// hook registered, a message delivers immediately with no boundary re-run.
func TestExecute_QueueNodeMessageDeliversLiveVsBoundary(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	var forwarded string
	go func() {
		<-stub.started
		ex.SetNodeLiveSteer("chat", "n1", func(text string) bool {
			forwarded = text
			return true
		})
		m, ok := ex.QueueNodeMessage("chat", "n1", "focus on cost")
		if !ok {
			t.Error("QueueNodeMessage returned false for a LIVE node")
		}
		if !m.Delivered {
			t.Error("message forwarded via a live steer hook should be marked delivered immediately")
		}
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	if forwarded != "focus on cost" {
		t.Errorf("live steer hook never got the message; got %q", forwarded)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.workerCalls != 1 {
		t.Errorf("worker ran %d times; a live-delivered steer must not also trigger a boundary re-run", stub.workerCalls)
	}
}

// TestExecute_PauseNodeStopsBeforeJudgeAndKeepsAnswer: pausing a running node
// stops it at its next gate-stage boundary (like cancel) but the answer
// propagates as a paused node, resumable - not cancelled.
func TestExecute_PauseNodeStopsBeforeJudge(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	go func() {
		<-stub.started
		if !ex.PauseNode("chat", "n1", PauseUser) {
			t.Error("PauseNode returned false for a LIVE node")
		}
		close(stub.unblock)
	}()
	events, _ := runPlanSSE(t, ex, plan, "chat")

	stub.mu.Lock()
	if stub.judgeCalls != 0 {
		t.Errorf("judge ran %d times; pause should have stopped the node before judging", stub.judgeCalls)
	}
	stub.mu.Unlock()
	if got := nodeEnd(events, "n1"); got != stream.EventNodePaused {
		t.Errorf("n1 ended as %q; want node_paused", got)
	}
}

// TestExecute_CancelNodeReportsDelivery: CancelNode tells the truth about whether
// it reached a live node - the API's 6x-"200 OK, node kept running" lie (live,
// 2026-07-13) started with the handler discarding this bool. NodeCancelled is the
// same fact, queryable by the tool layer.
func TestExecute_CancelNodeReportsDelivery(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	if ex.CancelNode("chat", "n1") {
		t.Error("CancelNode reported success before the node was even running")
	}
	if ex.NodeCancelled("chat", "n1") {
		t.Error("NodeCancelled true for a node nobody cancelled")
	}

	var delivered, seen bool
	go func() {
		<-stub.started
		delivered = ex.CancelNode("chat", "n1") // live node → must report true
		seen = ex.NodeCancelled("chat", "n1")   // …and be visible to the tool layer
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	if !delivered {
		t.Error("CancelNode returned false for a LIVE node")
	}
	if !seen {
		t.Error("NodeCancelled false right after a delivered cancel - a cancelled node's tools would keep running")
	}
	if ex.CancelNode("chat", "nope") {
		t.Error("CancelNode reported success for a node that isn't running")
	}
}

// TestExecute_QueueNodeMessageReportsDelivery: queueing a message aimed at a
// genuinely running node is delivered (ok=true) and picked up; one aimed at
// nothing is not (ok=false, which the handler surfaces as 404).
func TestExecute_QueueNodeMessageReportsDelivery(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	var delivered bool
	go func() {
		<-stub.started
		_, delivered = ex.QueueNodeMessage("chat", "n1", "focus on cost")
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	if !delivered {
		t.Fatal("QueueNodeMessage returned false for a LIVE node")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.workerCalls != 2 || !strings.Contains(stub.prompts[1], "focus on cost") {
		t.Errorf("queued message not picked up: %d worker calls, prompts=%v", stub.workerCalls, stub.prompts)
	}
	if _, ok := ex.QueueNodeMessage("chat", "n1", "too late"); ok {
		t.Error("QueueNodeMessage reported success after the node finished")
	}
}

// TestExecute_EditRemoveQueuedMessage: a not-yet-delivered queued message can
// be edited or removed; a delivered one is immutable.
func TestExecute_EditRemoveQueuedMessage(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)

	go func() {
		<-stub.started
		m, ok := ex.QueueNodeMessage("chat", "n1", "first draft")
		if !ok {
			t.Error("QueueNodeMessage returned false for a LIVE node")
		}
		if !ex.EditQueuedMessage("chat", "n1", m.ID, "edited") {
			t.Error("EditQueuedMessage failed on a not-yet-delivered message")
		}
		removable, ok2 := ex.QueueNodeMessage("chat", "n1", "will be removed")
		if !ok2 {
			t.Error("QueueNodeMessage returned false for a LIVE node")
		}
		if !ex.RemoveQueuedMessage("chat", "n1", removable.ID) {
			t.Error("RemoveQueuedMessage failed on a not-yet-delivered message")
		}
		close(stub.unblock)
	}()
	drain(t, ex, plan)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !strings.Contains(stub.prompts[1], "edited") {
		t.Errorf("re-run prompt missing the edited message: %q", stub.prompts[1])
	}
	if strings.Contains(stub.prompts[1], "will be removed") {
		t.Errorf("re-run prompt still contains the removed message: %q", stub.prompts[1])
	}
}

// TestQueuedMsg_StatusTransitions (steer-status enum): covers the four ways a
// message's Status is set, including deriving from a pre-enum persisted row
// that only carried the old Delivered bool.
func TestQueuedMsg_StatusTransitions(t *testing.T) {
	t.Run("enqueue without a live round queues", func(t *testing.T) {
		c := &nodeControl{}
		m := c.enqueue("hello")
		if m.Status != MsgQueued {
			t.Errorf("Status = %q, want %q", m.Status, MsgQueued)
		}
	})

	t.Run("enqueue with a live forward hook forwards", func(t *testing.T) {
		c := &nodeControl{}
		c.setLiveSteer(func(string) bool { return true })
		m := c.enqueue("hello")
		if m.Status != MsgForwarded {
			t.Errorf("Status = %q, want %q", m.Status, MsgForwarded)
		}
	})

	t.Run("gate drain marks drained", func(t *testing.T) {
		c := &nodeControl{}
		c.enqueue("hello")
		if got := c.TakeQueued(); got != "hello" {
			t.Fatalf("TakeQueued() = %q, want %q", got, "hello")
		}
		if c.queue[0].Status != MsgDrained {
			t.Errorf("Status = %q, want %q", c.queue[0].Status, MsgDrained)
		}
	})

	t.Run("a pre-enum persisted row derives its status from Delivered", func(t *testing.T) {
		cases := []struct {
			name string
			json string
			want MsgStatus
		}{
			{"delivered true derives drained", `{"ID":"a","Text":"x","Delivered":true}`, MsgDrained},
			{"delivered false derives queued", `{"ID":"a","Text":"x","Delivered":false}`, MsgQueued},
			{"no delivered field derives queued", `{"ID":"a","Text":"x"}`, MsgQueued},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var m queuedMsg
				if err := json.Unmarshal([]byte(tc.json), &m); err != nil {
					t.Fatalf("Unmarshal: %v", err)
				}
				if m.Status != tc.want {
					t.Errorf("Status = %q, want %q", m.Status, tc.want)
				}
			})
		}
	})
}

// A row written by this binary must stay correct when an OLDER binary reads it
// back after a rollback: without the legacy bool it would look still-queued and
// be re-delivered to the node.
func TestQueuedMsg_MarshalKeepsLegacyDeliveredForRollback(t *testing.T) {
	for _, tc := range []struct {
		status MsgStatus
		want   bool
	}{{MsgQueued, false}, {MsgForwarded, true}, {MsgDrained, true}} {
		b, err := json.Marshal(queuedMsg{ID: "m1", Text: "hi", Status: tc.status})
		if err != nil {
			t.Fatalf("marshal(%s): %v", tc.status, err)
		}
		var old struct {
			ID        string
			Text      string
			Delivered bool
		}
		if err := json.Unmarshal(b, &old); err != nil {
			t.Fatalf("old-binary unmarshal(%s): %v", tc.status, err)
		}
		if old.Delivered != tc.want {
			t.Errorf("status %s -> old Delivered = %v, want %v (rollback would re-deliver)", tc.status, old.Delivered, tc.want)
		}
		// And the new shape must still round-trip through our own decoder.
		var back queuedMsg
		if err := json.Unmarshal(b, &back); err != nil || back.Status != tc.status {
			t.Errorf("round-trip(%s) = %q, err %v", tc.status, back.Status, err)
		}
	}
}

// TestCancelNode_FiresRegisteredRoundAbort (#1030): CancelNode must reach a
// round already mid-flight via the abort func an ACP round registers with
// SetNodeRoundAbort, not just the cancelled flag - that flag alone is
// invisible to acp.Agent.round until the round returns on its own.
func TestCancelNode_FiresRegisteredRoundAbort(t *testing.T) {
	ex := &Executor{controls: newRunControls()}
	ex.controls.register("chat", "n1")

	fired := make(chan struct{}, 1)
	ex.SetNodeRoundAbort("chat", "n1", func() { fired <- struct{}{} })

	if !ex.CancelNode("chat", "n1") {
		t.Fatal("CancelNode returned false for a registered node")
	}
	select {
	case <-fired:
	default:
		t.Fatal("CancelNode did not fire the registered round-abort func")
	}
}

// TestCancelNode_RoundAbortIdempotentAndUnregistered: a cancel that arrives
// before any round registers an abort func must not panic (falls back to
// the cooperative flag, checked at the next boundary as before); a cancel
// after the abort func is cleared (round already ended) is likewise a
// harmless no-op.
func TestCancelNode_RoundAbortIdempotentAndUnregistered(t *testing.T) {
	ex := &Executor{controls: newRunControls()}
	ex.controls.register("chat", "n1")

	// No round registered yet - CancelNode must still succeed and set the flag.
	if !ex.CancelNode("chat", "n1") {
		t.Fatal("CancelNode returned false")
	}
	if !ex.NodeCancelled("chat", "n1") {
		t.Fatal("cancelled flag not set")
	}

	// A late registration on an already-cancelled node fires immediately -
	// the round must not be left to run to its own conclusion.
	fired := make(chan struct{}, 1)
	ex.SetNodeRoundAbort("chat", "n1", func() { fired <- struct{}{} })
	select {
	case <-fired:
	default:
		t.Fatal("SetNodeRoundAbort on an already-cancelled node did not fire immediately")
	}

	// Round end: unregister, then a second CancelNode call - must not panic.
	ex.ClearNodeRoundAbort("chat", "n1")
	if !ex.CancelNode("chat", "n1") {
		t.Fatal("second CancelNode call returned false")
	}
}
