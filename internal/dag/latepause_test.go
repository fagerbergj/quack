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
