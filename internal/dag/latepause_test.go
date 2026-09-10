package dag

import (
	"testing"

	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
)

// TestDagStream_LateOutputOverridesPause: a shutdown-drain pause flipped
// after the node already produced its answer (e.g. it landed inside
// commitDelivery, which runs before graph.go unregisters the control) must
// not suppress node_done - the PR/review is already posted, so dropping the
// output leaves dag_nodes at paused with no output and the boot sweep
// (ListPausedDagNodes) re-runs it. A live user pause is different: node.go's
// own cooperative check is what caught it, before delivery, so it still wins.
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

// TestDagStream_LiveUserPauseStillWinsOverDraftOutput: a user pause caught by
// node.go's own cooperative check (not a late shutdown race) still reports
// node_paused even though the node returns whatever draft answer it had -
// that draft was never delivered, so reporting node_done would be a lie.
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

// TestDagStream_DeliveredOutranksLivePauseRace closes the gap
// TestDagStream_LiveUserPauseStillWinsOverDraftOutput's own out!="" heuristic
// could not: a live user pause racing in AFTER commitDelivery already ran
// (not caught by node.go's own cooperative check beforehand) leaves the
// node's answer genuinely delivered - node.go's own MarkDelivered signal,
// not the ambiguous "is Output non-empty" guess, must win. The #1340 review
// only closed this race for PauseShutdown; a live PauseUser still needed it.
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

// TestDagStream_FinishSweepDeliveredOutranksEmptyOutputAndPause exercises
// Finish()'s own terminal sweep (review finding): the tests above all go
// through handle()+flush() with non-empty output, so none of them reach a
// node the sweep - not handle() - has to close out. The motivating case is a
// delivered answer that dedupeAnswerAgainstStaged collapsed to empty/
// whitespace, racing a pause that lands after commitDelivery: delivered must
// still outrank both the empty-output-means-failed guess and the pause flag,
// or dropping the `!delivered &&` guards on those branches would go
// unnoticed.
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
	// Never calling ds.handle: n1 is never doneEmitted, so Finish's own sweep
	// (not handle/flush) is what has to decide it, matching the motivating
	// race - a pause landing after commitDelivery, before graph.go's Finish.

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
