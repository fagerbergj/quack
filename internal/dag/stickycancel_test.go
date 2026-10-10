package dag

import (
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
)

// runControls.cancelled stays sticky past unregister, so retry must clear it or the stream
// discards the retried node's answer as cancelled.
func TestExecute_RetryClearsStaleCancelSticky(t *testing.T) {
	const chatID, nodeID = "chat-sticky", "n1"
	ex := NewExecutor(session.InMemoryService(), nil, nil, nil, nil, nil)

	// turn 1: node runs, user cancels it, the node closure returns (graph.go's defer).
	ex.controls.register(chatID, nodeID)
	if !ex.CancelNode(chatID, nodeID) {
		t.Fatal("setup: cancel should be accepted")
	}
	ex.controls.unregister(chatID, nodeID)

	// The retry path reaches register() without ResetNodeCancels, which runs only per turn.
	c, _, _ := ex.controls.register(chatID, nodeID)
	defer ex.controls.unregister(chatID, nodeID)
	if c.Cancelled() {
		t.Fatal("the fresh control should not be cancelled - the node really does run")
	}

	var got []stream.SSEEvent
	ds := newDagStream("", chatID, map[string]string{nodeID: "a"}, nil,
		func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
		map[string]string{},
		func(string) gateScore { return gateScore{} },
		func(string) bool { return ex.controls.wasCancelled(chatID, nodeID) },
		func(string) PauseReason { return ex.controls.pauseReason(chatID, nodeID) },
		func(string, int) string { return "" },
	)
	ds.handle(&session.Event{NodeInfo: &session.NodeInfo{Path: nodeID}, Output: "retried answer"})
	ds.flush()

	for _, e := range got {
		if e.Name == stream.EventNodeDone {
			return
		}
	}
	t.Fatalf("got %v; want node_done - a stale sticky cancel from the previous attempt discarded the retry's answer", names(got))
}
