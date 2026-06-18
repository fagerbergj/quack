package tools

import (
	"errors"
	"iter"
	"testing"

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

	// Completion: no node paused → no waiting node.
	if wn, qs, err := drainDAG(seqOf(stream.NodeDone("n1", stream.NodeDoneData{})), noop); err != nil || wn != "" || qs != nil {
		t.Errorf("completed run: got (%q, %v, %v), want (\"\", nil, nil)", wn, qs, err)
	}

	// Suspend: a node_waiting is captured (id + questions) → execute returns input_required.
	wn, qs, err := drainDAG(seqOf(stream.NodeWaiting(stream.NodeWaitingData{NodeID: "n2", Questions: []string{"which region?", "what budget?"}})), noop)
	if err != nil || wn != "n2" || len(qs) != 2 || qs[0] != "which region?" {
		t.Errorf("paused run: got (%q, %v, %v), want n2 + two questions", wn, qs, err)
	}

	// Forwarding: every event is emitted to the SSE seam.
	count := 0
	if _, _, err := drainDAG(seqOf(stream.NodeQueued("a"), stream.NodeStart("a", "ag")), func(stream.SSEEvent) { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("emit called %d times, want 2", count)
	}
}

func TestDrainDAGError(t *testing.T) {
	boom := func(yield func(stream.SSEEvent, error) bool) { yield(stream.SSEEvent{}, errors.New("boom")) }
	if _, _, err := drainDAG(boom, func(stream.SSEEvent) {}); err == nil {
		t.Error("want the stream error surfaced")
	}
}
