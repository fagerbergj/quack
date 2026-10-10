package dag

import (
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
)

// A shutdown pause flipped after the node already delivered must not suppress node_done,
// or the boot sweep re-runs it; a live user pause caught before delivery still wins.
func TestDagStream_LateOutputOverridesPause(t *testing.T) {
	agentByID := map[string]string{"n1": "a"}
	var got []stream.SSEEvent
	ds := newDagStream("", "", agentByID, nil,
		func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
		map[string]string{},
		func(string) gateScore { return gateScore{} },
		func(string) bool { return false },
		func(string) PauseReason { return PauseShutdown },
		func(string, int) string { return "" },
	)
	ds.handle(&session.Event{NodeInfo: &session.NodeInfo{Path: "n1"}, Output: "the delivered answer"})
	ds.flush()

	for _, e := range got {
		if e.Name == stream.EventNodeDone {
			return
		}
	}
	t.Fatalf("got %v; want node_done - a late shutdown pause must not discard a delivered answer", names(got))
}

// A user pause caught by node.go's cooperative check still reports node_paused: the draft
// was never delivered.
func TestDagStream_LiveUserPauseStillWinsOverDraftOutput(t *testing.T) {
	agentByID := map[string]string{"n1": "a"}
	var got []stream.SSEEvent
	ds := newDagStream("", "", agentByID, nil,
		func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
		map[string]string{},
		func(string) gateScore { return gateScore{} },
		func(string) bool { return false },
		func(string) PauseReason { return PauseUser },
		func(string, int) string { return "" },
	)
	ds.handle(&session.Event{NodeInfo: &session.NodeInfo{Path: "n1"}, Output: "an undelivered draft"})
	ds.flush()

	for _, e := range got {
		if e.Name == stream.EventNodePaused {
			return
		}
	}
	t.Fatalf("got %v; want node_paused - a live user pause must not be reported as delivered", names(got))
}

// A live user pause racing in after commitDelivery: node.go's MarkDelivered signal, not
// the "is Output non-empty" guess, must win.
func TestDagStream_DeliveredOutranksLivePauseRace(t *testing.T) {
	agentByID := map[string]string{"n1": "a"}
	var got []stream.SSEEvent
	ds := newDagStream("", "", agentByID, nil,
		func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
		map[string]string{},
		func(string) gateScore { return gateScore{} },
		func(string) bool { return false },
		func(string) PauseReason { return PauseUser },
		func(string, int) string { return "" },
	)
	ds.deliveredOf = func(string) bool { return true }
	ds.handle(&session.Event{NodeInfo: &session.NodeInfo{Path: "n1"}, Output: "the delivered answer"})
	ds.flush()

	for _, e := range got {
		if e.Name == stream.EventNodeDone {
			return
		}
	}
	t.Fatalf("got %v; want node_done - MarkDelivered must outrank a pause that raced in after commitDelivery", names(got))
}

// Finish's own sweep: a delivered answer deduped to empty, racing a late pause, must
// still be done, not failed or paused.
func TestDagStream_FinishSweepDeliveredOutranksEmptyOutputAndPause(t *testing.T) {
	agentByID := map[string]string{"n1": "a"}
	var got []stream.SSEEvent
	ds := newDagStream("", "", agentByID, nil,
		func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
		map[string]string{}, // outputs: n1 stays unset - the empty-output case
		func(string) gateScore { return gateScore{} },
		func(string) bool { return false },
		func(string) PauseReason { return PauseUser },
		func(string, int) string { return "" },
	)
	ds.deliveredOf = func(string) bool { return true }
	// Never calling ds.handle leaves n1 for Finish's sweep to decide.

	s := &DagStream{
		ds:        ds,
		plan:      Plan{Nodes: []Node{{ID: "n1"}}},
		agentByID: agentByID,
		yield:     func(ev stream.SSEEvent, _ error) bool { got = append(got, ev); return true },
	}
	s.Finish()

	for _, e := range got {
		switch e.Name {
		case stream.EventNodeDone:
			return
		case stream.EventNodePaused, stream.EventNodeFailed:
			t.Fatalf("got %s; want node_done - delivered must outrank the pause flag and the empty-output guess", e.Name)
		}
	}
	t.Fatalf("got %v; want node_done", names(got))
}
