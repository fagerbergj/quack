package tools

import (
	"errors"
	"iter"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

func seqOf(evs ...stream.SSEEvent) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		for _, e := range evs {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func TestDrainDAG(t *testing.T) {
	noop := func(stream.SSEEvent) {}

	// Completion: no node paused.
	if w, err := drainDAG(seqOf(stream.NodeDone("n1", stream.NodeDoneData{})), noop); err != nil || len(w) != 0 {
		t.Errorf("completed run: got (%v, %v), want (nil, nil)", w, err)
	}

	// Suspend: every node_waiting is captured (id + call + questions).
	w, err := drainDAG(seqOf(stream.NodeWaiting(stream.NodeWaitingData{NodeID: "n2", CallID: "c2", Questions: []string{"which region?"}})), noop)
	if err != nil || len(w) != 1 || w[0].NodeID != "n2" || w[0].CallID != "c2" || len(w[0].Questions) != 1 {
		t.Errorf("paused run: got (%+v, %v), want one waiting node n2/c2", w, err)
	}

	// Forwarding: every event is emitted to the SSE seam.
	count := 0
	if _, err := drainDAG(seqOf(stream.NodeQueued("a"), stream.NodeStart("a", "ag")), func(stream.SSEEvent) { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("emit called %d times, want 2", count)
	}
}

func TestDrainDAGError(t *testing.T) {
	boom := func(yield func(stream.SSEEvent, error) bool) { yield(stream.SSEEvent{}, errors.New("boom")) }
	if _, err := drainDAG(boom, func(stream.SSEEvent) {}); err == nil {
		t.Error("want the stream error surfaced")
	}
}

// TestBuildPending checks the suspend snapshot: the first waiting node is the resume
// target, the rest stay blocked, and non-waiting outputs are the done set.
func TestBuildPending(t *testing.T) {
	plan := dag.Plan{ID: "p1", Nodes: []dag.Node{{ID: "A"}, {ID: "B"}, {ID: "C"}}}
	outputs := map[string]string{"A": "out-A", "B": "partial-B", "C": "partial-C"}
	waiting := []stream.NodeWaitingData{
		{NodeID: "B", CallID: "cB"},
		{NodeID: "C", CallID: "cC"},
	}
	p := buildPending(plan, outputs, waiting)

	if p.NodeID != "B" || p.CallID != "cB" {
		t.Errorf("resume target = %s/%s, want B/cB (first waiting)", p.NodeID, p.CallID)
	}
	if !p.Waiting["C"] || p.Waiting["B"] {
		t.Errorf("waiting set = %v, want {C} (others, not the target)", p.Waiting)
	}
	if p.Done["A"] != "out-A" || len(p.Done) != 1 {
		t.Errorf("done = %v, want only the non-waiting node A", p.Done)
	}
}
