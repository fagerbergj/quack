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
	counts, failed := collectExec(t, exec.Execute(context.Background(), plan, "user", nodeOutputs))

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
	_, failed := collectExec(t, exec.Execute(context.Background(), plan, "user", nodeOutputs))

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
