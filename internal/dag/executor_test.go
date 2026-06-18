package dag

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

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

// waitingAgent builds a custom ADK agent that pauses: it yields a request_input
// FunctionCall with its ID listed in LongRunningToolIDs (what ADK + the A2A v2
// bridge produce for a long-running call), so the executor detects it as waiting.
func waitingAgent(t *testing.T, name string, questions []string, rec *recorder) adkagent.Agent {
	t.Helper()
	ag, err := adkagent.New(adkagent.Config{
		Name:        name,
		Description: "fake waiting agent",
		Run: func(ic adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				rec.record(name, "")
				ev := session.NewEvent(ic.InvocationID())
				ev.Author = name
				qs := make([]any, len(questions))
				for i, q := range questions {
					qs[i] = q
				}
				ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{ID: name + "-call", Name: "request_input", Args: map[string]any{"questions": qs}},
				}}}
				ev.LongRunningToolIDs = []string{name + "-call"}
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

// collectExec drains an Execute stream into event-name counts, per-node failure
// messages, and per-node waiting payloads, failing on an unexpected stream error.
func collectExec(t *testing.T, seq iter.Seq2[stream.SSEEvent, error]) (counts map[string]int, failed map[string]string, waiting map[string]stream.NodeWaitingData) {
	t.Helper()
	counts = map[string]int{}
	failed = map[string]string{}
	waiting = map[string]stream.NodeWaitingData{}
	for ev, err := range seq {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		counts[ev.Name]++
		switch d := ev.Data.(type) {
		case stream.NodeFailedData:
			failed[d.NodeID] = d.Error
		case stream.NodeWaitingData:
			waiting[d.NodeID] = d
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
	counts, failed, _ := collectExec(t, exec.Execute(context.Background(), plan, "user", nodeOutputs))

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
	_, failed, _ := collectExec(t, exec.Execute(context.Background(), plan, "user", nodeOutputs))

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

// TestExecuteSuspendOnWaiting asserts a node that pauses on request_input emits
// node_waiting, an independent branch still completes, the blocked dependent
// never runs, and Execute returns (the DAG suspends without hanging).
func TestExecuteSuspendOnWaiting(t *testing.T) {
	rec := newRecorder()
	clients := map[string]adkagent.Agent{
		"agA": fakeAgent(t, "agA", "out-A", rec, false),
		"agB": waitingAgent(t, "agB", []string{"Which region?", "What budget?"}, rec), // pauses with two questions
		"agC": fakeAgent(t, "agC", "out-C", rec, false),                               // independent branch
		"agD": fakeAgent(t, "agD", "out-D", rec, false),                               // depends on the paused B
	}
	exec := NewExecutor(session.InMemoryService(), clients, nil, 4)
	plan := Plan{
		ID:          "p3",
		UserMessage: "hi",
		Nodes: []Node{
			{ID: "A", AgentName: "agA", Task: "do A"},
			{ID: "B", AgentName: "agB", Task: "do B", DependsOn: []string{"A"}},
			{ID: "C", AgentName: "agC", Task: "do C"}, // independent of B
			{ID: "D", AgentName: "agD", Task: "do D", DependsOn: []string{"B"}},
		},
	}

	nodeOutputs := map[string]string{}
	counts, failed, waiting := collectExec(t, exec.Execute(context.Background(), plan, "user", nodeOutputs))

	if len(failed) != 0 {
		t.Fatalf("unexpected node failures: %v", failed)
	}
	if counts[stream.EventNodeWaiting] != 1 {
		t.Errorf("node_waiting count = %d, want 1", counts[stream.EventNodeWaiting])
	}
	w, ok := waiting["B"]
	if !ok {
		t.Fatalf("expected node_waiting for B, got %v", waiting)
	}
	if len(w.Questions) != 2 || w.Questions[0] != "Which region?" || w.Questions[1] != "What budget?" || w.CallID == "" {
		t.Errorf("waiting payload = %+v, want two questions + call id", w)
	}
	if rec.order["agC"] == 0 {
		t.Errorf("independent node C should have run while B waited: order=%v", rec.order)
	}
	if rec.order["agD"] != 0 {
		t.Errorf("D must not run while its dependency B is waiting: order=%v", rec.order)
	}
}
