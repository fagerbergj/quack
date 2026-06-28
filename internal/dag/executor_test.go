package dag

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// recorder captures the order in which fake node agents ran and the task text
// each received, so a test can assert dependency ordering and downstream
// rehydration (an upstream node's output reaching a dependent's task).
type recorder struct {
	mu    sync.Mutex
	seq   int
	order map[string]int    // node agent name → run sequence number (1-based)
	tasks map[string]string // node agent name → task text it received
}

func newRecorder() *recorder {
	return &recorder{order: map[string]int{}, tasks: map[string]string{}}
}

func (r *recorder) record(name, task string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.order[name] = r.seq
	r.tasks[name] = task
}

// fakeAgent builds a custom ADK agent that records its run and yields `output`
// as a plain-text answer (or errors when fail is true). Used as an executor
// client so scheduling can be tested without the A2A/gate/model stack.
func fakeAgent(t *testing.T, name, output string, rec *recorder, fail bool) adkagent.Agent {
	t.Helper()
	ag, err := adkagent.New(adkagent.Config{
		Name:        name,
		Description: "fake test agent",
		Run: func(ic adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				task := ""
				if uc := ic.UserContent(); uc != nil && len(uc.Parts) > 0 {
					task = uc.Parts[0].Text
				}
				rec.record(name, task)
				if fail {
					yield(nil, fmt.Errorf("node agent %q boom", name))
					return
				}
				ev := session.NewEvent(ic.InvocationID())
				ev.Author = name
				ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: output}}}
				ev.TurnComplete = true
				yield(ev, nil)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// collectExec drains an Execute stream into event-name counts and per-node
// failure messages, failing on an unexpected stream error.
func collectExec(t *testing.T, seq iter.Seq2[stream.SSEEvent, error]) (counts map[string]int, failed map[string]string) {
	t.Helper()
	counts = map[string]int{}
	failed = map[string]string{}
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		counts[ev.Name]++
		if d, ok := ev.Data.(stream.NodeFailedData); ok {
			failed[d.NodeID] = d.Error
		}
	}
	return
}

// TestExecuteReadinessDiamond runs a diamond A → {B,C} → D and asserts nodes
// fire by readiness (A before B/C, D after both) and that a dependent's task is
// rehydrated with its upstreams' outputs.
func TestExecuteReadinessDiamond(t *testing.T) {
	rec := newRecorder()
	clients := map[string]adkagent.Agent{
		"agA": fakeAgent(t, "agA", "out-A", rec, false),
		"agB": fakeAgent(t, "agB", "out-B", rec, false),
		"agC": fakeAgent(t, "agC", "out-C", rec, false),
		"agD": fakeAgent(t, "agD", "out-D", rec, false),
	}
	exec := NewExecutor(session.InMemoryService(), clients, nil, 4)
	plan := Plan{
		ID:          "p1",
		UserMessage: "hi",
		Nodes: []Node{
			{ID: "A", AgentName: "agA", Task: "do A"},
			{ID: "B", AgentName: "agB", Task: "do B", DependsOn: []string{"A"}},
			{ID: "C", AgentName: "agC", Task: "do C", DependsOn: []string{"A"}},
			{ID: "D", AgentName: "agD", Task: "do D", DependsOn: []string{"B", "C"}},
		},
	}

	nodeOutputs := map[string]string{}
	counts, failed := collectExec(t, exec.Execute(context.Background(), plan, "user", "chat1", nodeOutputs))

	if len(failed) != 0 {
		t.Fatalf("unexpected node failures: %v", failed)
	}
	if counts[stream.EventNodeDone] != 4 {
		t.Errorf("node_done count = %d, want 4", counts[stream.EventNodeDone])
	}
	for _, id := range []string{"A", "B", "C", "D"} {
		if got := nodeOutputs[id]; got != "out-"+id {
			t.Errorf("nodeOutputs[%s] = %q, want %q", id, got, "out-"+id)
		}
	}

	// Readiness ordering: A runs before its dependents; D runs after both of its.
	if !(rec.order["agA"] < rec.order["agB"] && rec.order["agA"] < rec.order["agC"]) {
		t.Errorf("A should run before B and C: order=%v", rec.order)
	}
	if !(rec.order["agD"] > rec.order["agB"] && rec.order["agD"] > rec.order["agC"]) {
		t.Errorf("D should run after B and C: order=%v", rec.order)
	}

	// Downstream rehydration: D's task carries B's and C's outputs.
	if td := rec.tasks["agD"]; !strings.Contains(td, "out-B") || !strings.Contains(td, "out-C") {
		t.Errorf("D task missing upstream outputs: %q", td)
	}
}

// blockingAgent records its start (signalling `started`) then blocks until its
// context is cancelled — so a test can cancel it mid-run via CancelNode.
func blockingAgent(t *testing.T, name string, rec *recorder, started chan<- struct{}) adkagent.Agent {
	t.Helper()
	ag, err := adkagent.New(adkagent.Config{
		Name:        name,
		Description: "blocking fake test agent",
		Run: func(ic adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				rec.record(name, "")
				select {
				case started <- struct{}{}:
				default:
				}
				<-ic.Done() // unblocks when this node's ctx is cancelled
				yield(nil, ic.Err())
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

// TestExecuteCancelNodeContinues: cancelling ONE running node ends that node
// (node_failed: "cancelled by user") but the run continues — its dependent still
// executes (continue-but-warn), unlike a node error which halts the DAG.
func TestExecuteCancelNodeContinues(t *testing.T) {
	rec := newRecorder()
	startedB := make(chan struct{}, 1)
	clients := map[string]adkagent.Agent{
		"agA": fakeAgent(t, "agA", "out-A", rec, false),
		"agB": blockingAgent(t, "agB", rec, startedB),
		"agC": fakeAgent(t, "agC", "out-C", rec, false),
	}
	exec := NewExecutor(session.InMemoryService(), clients, nil, 4)
	plan := Plan{
		ID:          "pC",
		UserMessage: "hi",
		Nodes: []Node{
			{ID: "A", AgentName: "agA", Task: "do A"},
			{ID: "B", AgentName: "agB", Task: "do B", DependsOn: []string{"A"}},
			{ID: "C", AgentName: "agC", Task: "do C", DependsOn: []string{"B"}},
		},
	}

	type res struct {
		counts map[string]int
		failed map[string]string
	}
	done := make(chan res, 1)
	go func() {
		counts := map[string]int{}
		failed := map[string]string{}
		for ev, err := range exec.Execute(context.Background(), plan, "user", "chatX", map[string]string{}) {
			if err != nil {
				continue
			}
			counts[ev.Name]++
			if d, ok := ev.Data.(stream.NodeFailedData); ok {
				failed[d.NodeID] = d.Error
			}
		}
		done <- res{counts, failed}
	}()

	<-startedB // B is running
	if !exec.CancelNode("chatX", "B") {
		t.Fatal("CancelNode should find running node B")
	}
	// Cancelling an unknown node/chat is a safe no-op.
	if exec.CancelNode("chatX", "ghost") || exec.CancelNode("nope", "B") {
		t.Error("CancelNode on a missing node/chat should return false")
	}

	select {
	case r := <-done:
		if r.failed["B"] == "" {
			t.Errorf("B should be reported cancelled, failures=%v", r.failed)
		}
		if rec.order["agC"] == 0 {
			t.Error("C must still run after B is cancelled (continue-but-warn)")
		}
		if r.counts[stream.EventNodeDone] != 2 { // A and C
			t.Errorf("node_done = %d, want 2 (A + C)", r.counts[stream.EventNodeDone])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not complete after cancelling a node (deadlock?)")
	}
}

// TestExecuteFailureStopsDownstream asserts a node error halts the DAG: the
// failed node emits node_failed, downstream never runs, and Execute returns.
func TestExecuteFailureStopsDownstream(t *testing.T) {
	rec := newRecorder()
	clients := map[string]adkagent.Agent{
		"agA": fakeAgent(t, "agA", "out-A", rec, false),
		"agB": fakeAgent(t, "agB", "", rec, true), // errors
		"agC": fakeAgent(t, "agC", "out-C", rec, false),
	}
	exec := NewExecutor(session.InMemoryService(), clients, nil, 2)
	plan := Plan{
		ID:          "p2",
		UserMessage: "hi",
		Nodes: []Node{
			{ID: "A", AgentName: "agA", Task: "do A"},
			{ID: "B", AgentName: "agB", Task: "do B", DependsOn: []string{"A"}},
			{ID: "C", AgentName: "agC", Task: "do C", DependsOn: []string{"B"}},
		},
	}

	nodeOutputs := map[string]string{}
	_, failed := collectExec(t, exec.Execute(context.Background(), plan, "user", "chat1", nodeOutputs))

	if _, ok := failed["B"]; !ok {
		t.Errorf("expected node_failed for B, got failures=%v", failed)
	}
	if _, ok := rec.order["agC"]; ok {
		t.Errorf("C must not run after its dependency B failed: order=%v", rec.order)
	}
	if rec.order["agA"] == 0 {
		t.Errorf("A should have run before B failed: order=%v", rec.order)
	}
}
