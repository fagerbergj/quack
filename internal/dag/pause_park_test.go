package dag

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

var coopChain = Plan{ID: "t", UserMessage: "x", Nodes: []Node{
	{ID: "n1", AgentName: "blk", Task: "research"},
	{ID: "n2", AgentName: "blk", Task: "compare", DependsOn: []string{"n1"}},
}}

// pauseFirstWorker user-pauses n1 while its first worker call is in flight, then lets the call return.
func pauseFirstWorker(t *testing.T, ex *Executor, stub *coopStub) {
	t.Helper()
	go func() {
		<-stub.started
		if !ex.PauseNode("chat", "n1", PauseUser) {
			t.Error("PauseNode found no live n1")
		}
		close(stub.unblock)
	}()
}

type eventLog struct {
	mu     sync.Mutex
	events []stream.SSEEvent
}

func (l *eventLog) add(ev stream.SSEEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

func (l *eventLog) started(nodeID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.events {
		if d, ok := ev.Data.(stream.NodeStartData); ok && d.NodeID == nodeID {
			return true
		}
	}
	return false
}

// An incremental step: a user-paused node parks like a HITL one, and its dependent never starts.
func TestRunPlanStep_UserPausedNodeParksAndHoldsDependents(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, _ := newCoopExecutor(t, stub, 1)
	pauseFirstWorker(t, ex, stub)
	log := &eventLog{}
	ctx := stream.WithYield(context.Background(), log.add)

	outputs, parked, started, err := ex.RunPlanStep(ctx, coopChain, "quack", "u", "chat", nil, map[string]bool{"n1": true, "n2": true})
	if err != nil {
		t.Fatalf("RunPlanStep: %v", err)
	}
	if !parked["n1"] || outputs["n1"] != "" {
		t.Errorf("n1 parked=%v output=%q; want parked with no output", parked["n1"], outputs["n1"])
	}
	if started["n2"] || log.started("n2") {
		t.Error("n2 started while its dependency n1 was paused")
	}
	if got := nodeEnd(log.events, "n1"); got != stream.EventNodePaused {
		t.Errorf("n1 ended as %q; want node_paused", got)
	}
	if got := nodeEnd(log.events, "n2"); got != "" {
		t.Errorf("never-started n2 ended as %q; want no terminal", got)
	}
}

// A bound (native graph) run: the paused node holds its dependent and the run reports paused.
func TestRunPlanAsGraph_UserPausedNodeParksAndHoldsDependents(t *testing.T) {
	stub := &coopStub{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	ex, _ := newCoopExecutor(t, stub, 1)
	pauseFirstWorker(t, ex, stub)
	log := &eventLog{}
	ctx := stream.WithYield(context.Background(), log.add)

	paused, err := ex.RunPlanAsGraph(ctx, coopChain, "quack", "u", "chat", genai.NewContentFromText("x", genai.RoleUser),
		func(ev stream.SSEEvent, _ error) bool { log.add(ev); return true }, map[string]string{}, nil)
	if err != nil {
		t.Fatalf("RunPlanAsGraph: %v", err)
	}
	if !paused {
		t.Error("run with a paused node reported not paused; its caller would finalize a partial answer")
	}
	if log.started("n2") {
		t.Error("n2 started while its dependency n1 was paused")
	}
	if got := nodeEnd(log.events, "n1"); got != stream.EventNodePaused {
		t.Errorf("n1 ended as %q; want node_paused", got)
	}
	if got := nodeEnd(log.events, "n2"); got != "" {
		t.Errorf("never-started n2 ended as %q; want no terminal", got)
	}
}
