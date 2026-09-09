package rest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// staleRunMarker tags the stale previous run's events so a test can tell
// "the new run legitimately produced its own events already" (fine) apart
// from "the previous run's stale events leaked through" (the bug).
const staleRunMarker = "STALE-PREVIOUS-RUN-MARKER"

// seedStaleTerminalRun leaves a finished run's durable events - including its
// terminal `done` - on chatID, standing in for whatever the chat's previous
// run left behind.
func seedStaleTerminalRun(t *testing.T, h *Handler, chatID string) {
	t.Helper()
	ctx := context.Background()
	for i, ev := range []stream.SSEEvent{stream.Errorf(staleRunMarker), stream.Done()} {
		js, err := runlog.MarshalEvent(ev)
		if err != nil {
			t.Fatalf("marshal stale event %d: %v", i, err)
		}
		if err := h.store.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: int64(i + 1), Event: js}); err != nil {
			t.Fatalf("seed stale event %d: %v", i, err)
		}
	}
}

// assertEventLogAlreadyReset fails if chatID's durable event log still
// carries the stale marker - the state it must be in the instant a
// run-starting handler responds, well before its background goroutine gets a
// chance to run. It does not require the log to be totally empty: the new
// run's own goroutine may already have raced ahead and appended its own
// (legitimate) events by the time this runs.
func assertEventLogAlreadyReset(t *testing.T, h *Handler, chatID string) {
	t.Helper()
	evs, err := h.eventLog.LoadEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Event, staleRunMarker) {
			t.Errorf("stale previous run's marker event was still present immediately after the handler "+
				"responded; the reset must run synchronously before responding, not from inside the run's "+
				"own spawned goroutine: %q", ev.Event)
			return
		}
	}
}

// TestStartNode_AwaitingInputResetsBeforeResponding pins finding 5: a
// subscriber landing in a resumed node's start window must never replay the
// previous run's terminal `done`. startNodeAsync used to reset the durable
// event log from inside its own spawned goroutine, so a subscriber racing
// the 200 could read the stale run straight off the durable table.
func TestStartNode_AwaitingInputResetsBeforeResponding(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "needs_input", PendingQuestion: "which region?"}); err != nil {
		t.Fatalf("seed parked node: %v", err)
	}
	seedStaleTerminalRun(t, h, chatID)

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{Content: strPtr("the answer")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertEventLogAlreadyReset(t, h, chatID)
}

// TestUpdateNodeStatus_RetryResetsBeforeResponding is
// TestStartNode_AwaitingInputResetsBeforeResponding's counterpart for
// retryNodeAsync (queued/paused -> running via PUT status).
func TestUpdateNodeStatus_RetryResetsBeforeResponding(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "queued"}); err != nil {
		t.Fatalf("seed queued node: %v", err)
	}
	seedStaleTerminalRun(t, h, chatID)

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertEventLogAlreadyReset(t, h, chatID)
}

// TestSubscribeDuringNodeStartWindow drives the full symptom end to end: a
// subscriber connecting right after the 200 - before the resumed run has
// published anything - must not be served the previous run's terminal done.
func TestSubscribeDuringNodeStartWindow(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "queued"}); err != nil {
		t.Fatalf("seed queued node: %v", err)
	}
	seedStaleTerminalRun(t, h, chatID)

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := subscribe(t, h, chatID, "")
	if strings.Contains(body, staleRunMarker) {
		t.Errorf("subscriber in the start window was served the previous run's stale event/terminal done: %q", body)
	}
}
