package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// fakeNodeStore is an in-memory NodeStateStore: what dag_nodes' lifecycle
// columns would hold, without a database.
type fakeNodeStore struct {
	mu                              sync.Mutex
	status, reason, question, queue map[string]string
}

func newFakeNodeStore() *fakeNodeStore {
	return &fakeNodeStore{
		status: map[string]string{}, reason: map[string]string{},
		question: map[string]string{}, queue: map[string]string{},
	}
}

// SetNodeStatusForChat enforces CanTransition like the real store, so an
// illegal write fails the test instead of silently landing.
func (f *fakeNodeStore) SetNodeStatusForChat(_ context.Context, chatID, nodeID, status, reason, question string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := chatID + "/" + nodeID
	if status != "" {
		from := NodeStatus(f.status[k])
		if from == "" {
			from = StatusQueued
		}
		if to := NodeStatus(status); from != to && !CanTransition(from, to) {
			return fmt.Errorf("fakeNodeStore: illegal transition %s → %s (node %s)", from, to, nodeID)
		}
		f.status[k] = status
	}
	f.reason[k], f.question[k] = reason, question
	return nil
}

func (f *fakeNodeStore) SetNodeQueue(_ context.Context, chatID, nodeID, queueJSON string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[chatID+"/"+nodeID] = queueJSON
	return nil
}

func (f *fakeNodeStore) GetNodeState(_ context.Context, chatID, nodeID string) (string, string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := chatID + "/" + nodeID
	return f.status[k], f.reason[k], f.question[k], f.queue[k], nil
}

// set stamps a row's status directly, standing in for the stream-driven
// node_start upsert (runlog), which isn't wired in these tests.
func (f *fakeNodeStore) set(chatID, nodeID string, status NodeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[chatID+"/"+nodeID] = string(status)
}

func (f *fakeNodeStore) get(chatID, nodeID string) (status, reason, question string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := chatID + "/" + nodeID
	return f.status[k], f.reason[k], f.question[k]
}

// TestNodeTransitions pins the lifecycle table: the legal moves the issue
// draws, and the illegal ones a stale client could ask for.
func TestNodeTransitions(t *testing.T) {
	legal := [][2]NodeStatus{
		{StatusQueued, StatusRunning},
		{StatusRunning, StatusPaused},
		{StatusPaused, StatusRunning},
		{StatusRunning, StatusDone},
		{StatusRunning, StatusFailed},
		{StatusQueued, StatusCancelled},
		{StatusRunning, StatusCancelled},
		{StatusPaused, StatusCancelled},
		{StatusDone, StatusQueued}, // retry
	}
	for _, c := range legal {
		if !CanTransition(c[0], c[1]) {
			t.Errorf("%s → %s should be legal", c[0], c[1])
		}
	}
	illegal := [][2]NodeStatus{
		{StatusDone, StatusRunning},
		{StatusDone, StatusPaused},
		{StatusCancelled, StatusRunning},
		{StatusFailed, StatusDone},
		{StatusQueued, StatusPaused},
		{StatusPaused, StatusDone},
	}
	for _, c := range illegal {
		if CanTransition(c[0], c[1]) {
			t.Errorf("%s → %s should be rejected", c[0], c[1])
		}
	}
}

// TestPauseNodePersistsReason: a user pause mid-node lands in the store as
// paused/user before it is acted on, and StartNode clears it.
func TestPauseNodePersistsReason(t *testing.T) {
	fake := newFakeNodeStore()
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)
	ex.SetNodeStateStore(fake)

	go func() {
		<-stub.started
		fake.set("chat", "n1", StatusRunning) // runlog's node_start would have landed by now
		if !ex.PauseNode("chat", "n1", PauseUser) {
			t.Error("PauseNode returned false for a LIVE node")
		}
		close(stub.unblock)
	}()
	events, _ := runPlanSSE(t, ex, plan, "chat")

	if got := nodeEnd(events, "n1"); got != stream.EventNodePaused {
		t.Fatalf("n1 ended as %q; want node_paused", got)
	}
	status, reason, _ := fake.get("chat", "n1")
	if status != string(StatusPaused) || reason != string(PauseUser) {
		t.Errorf("persisted (%q, %q); want (paused, user)", status, reason)
	}
	if got, ok := ex.StartNode("chat", "n1"); !ok || got != PauseUser {
		t.Errorf("StartNode = (%q, %v); want (user, true)", got, ok)
	}
	if r := ex.NodePauseReason("chat", "n1"); r != "" {
		t.Errorf("pause reason after StartNode = %q; want empty", r)
	}
}

// TestPauseForInputPersistsQuestion: the HITL park goes through the same
// markPaused seam and puts the question on the node, not the chat.
func TestPauseForInputPersistsQuestion(t *testing.T) {
	fake := newFakeNodeStore()
	c := &nodeControl{store: fake, chatID: "chat", nodeID: "n1"}
	c.PauseForInput("which region?")

	if !c.Paused() || c.PauseReason() != PauseAwaitingInput {
		t.Fatalf("control = (paused %v, reason %q); want (true, awaiting_input)", c.Paused(), c.PauseReason())
	}
	// Status is left to the needs_input event; the reason + question are ours.
	_, reason, q := fake.get("chat", "n1")
	if reason != string(PauseAwaitingInput) || q != "which region?" {
		t.Errorf("persisted (%q, %q); want (awaiting_input, \"which region?\")", reason, q)
	}
}

// TestQueuedSteerSurvivesRestart: enqueue, throw the in-memory control away
// (a process restart), rebuild from the persisted row - the undelivered
// message is still there and still deliverable.
func TestQueuedSteerSurvivesRestart(t *testing.T) {
	fake := newFakeNodeStore()
	controls := newRunControls()
	controls.store = fake

	c, _, _ := controls.register("chat", "n1")
	c.enqueue("focus on cost")
	c.persistQueue()
	if fake.queue["chat/n1"] == "" {
		t.Fatal("queue not persisted")
	}

	controls.unregister("chat", "n1")
	rebuilt := newRunControls()
	rebuilt.store = fake
	c2, _, _ := rebuilt.register("chat", "n1")

	if got := c2.TakeQueued(); got != "focus on cost" {
		t.Fatalf("restored queue drained %q; want the queued steer", got)
	}
	// Delivered messages must not come back a second time.
	c2.persistQueue()
	var msgs []queuedMsg
	if err := json.Unmarshal([]byte(fake.queue["chat/n1"]), &msgs); err != nil {
		t.Fatalf("queue json: %v", err)
	}
	if len(msgs) != 1 || !msgs[0].Delivered {
		t.Errorf("persisted queue = %+v; want one delivered message", msgs)
	}
}

// TestResumedNodeActuallyRuns: pause a live node (row lands paused/user),
// throw the executor away (restart), re-run the plan against the same store -
// the re-registered node must clear the persisted pause and do real work, not
// rehydrate the pause and re-park itself before its first worker round.
func TestResumedNodeActuallyRuns(t *testing.T) {
	fake := newFakeNodeStore()
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)
	ex.SetNodeStateStore(fake)

	go func() {
		<-stub.started
		fake.set("chat", "n1", StatusRunning) // runlog's node_start would have landed by now
		ex.PauseNode("chat", "n1", PauseUser)
		close(stub.unblock)
	}()
	events, _ := runPlanSSE(t, ex, plan, "chat")
	if got := nodeEnd(events, "n1"); got != stream.EventNodePaused {
		t.Fatalf("n1 ended as %q; want node_paused", got)
	}
	if status, _, _ := fake.get("chat", "n1"); status != string(StatusPaused) {
		t.Fatalf("persisted status %q; want paused", status)
	}

	// "Restart": fresh executor, same store, resume the node.
	stub2 := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	close(stub2.unblock)
	ex2, plan2 := newCoopExecutor(t, stub2, 1)
	ex2.SetNodeStateStore(fake)
	ex2.StartNode("chat", "n1")
	events2, _ := runPlanSSE(t, ex2, plan2, "chat")

	stub2.mu.Lock()
	workerRan := stub2.workerCalls
	stub2.mu.Unlock()
	if workerRan == 0 {
		t.Fatal("resumed node never reached its worker - it re-parked on the persisted pause")
	}
	if got := nodeEnd(events2, "n1"); got != stream.EventNodeDone {
		t.Fatalf("resumed n1 ended as %q; want node_done", got)
	}
	status, reason, _ := fake.get("chat", "n1")
	if status != string(StatusRunning) || reason != "" {
		t.Errorf("persisted (%q, %q) after resume; want (running, \"\")", status, reason)
	}
}

// TestStartNodeOnLiveControlPersistsRunning: StartNode against a LIVE control
// goes through resume(), whose store write (paused -> running) no other
// store-wired test reaches - both restart tests use a fresh executor.
func TestStartNodeOnLiveControlPersistsRunning(t *testing.T) {
	fake := newFakeNodeStore()
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, plan := newCoopExecutor(t, stub, 1)
	ex.SetNodeStateStore(fake)

	go func() {
		<-stub.started
		fake.set("chat", "n1", StatusRunning) // runlog's node_start would have landed by now
		ex.PauseNode("chat", "n1", PauseUser)
		if _, ok := ex.StartNode("chat", "n1"); !ok {
			t.Error("StartNode returned false for a live node")
		}
		close(stub.unblock)
	}()
	events, _ := runPlanSSE(t, ex, plan, "chat")

	if got := nodeEnd(events, "n1"); got != stream.EventNodeDone {
		t.Fatalf("n1 ended as %q; want node_done (pause was cleared before the gate)", got)
	}
	status, reason, _ := fake.get("chat", "n1")
	if status != string(StatusRunning) || reason != "" {
		t.Errorf("persisted (%q, %q); want (running, \"\") - resume() must persist the legal target", status, reason)
	}
}
