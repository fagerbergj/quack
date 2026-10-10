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

// staleRunMarker tags the previous run's events, so a test can tell the new run's own events
// apart from leaked stale ones.
const staleRunMarker = "STALE-PREVIOUS-RUN-MARKER"

// seedStaleTerminalRun leaves a finished run's durable events, including its terminal `done`, on chatID.
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

// assertEventLogAlreadyReset fails if the durable log still carries the stale marker when the handler
// responds. The log needn't be empty: the new run may already have appended its own events.
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

// TestStartNode_AwaitingInputResetsBeforeResponding: the durable log is reset before the 200, so a subscriber
// racing a resumed node's start never replays the previous run's terminal `done`.
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

// TestUpdateNodeStatus_RetryResetsBeforeResponding is the same check for retryNodeAsync
// (queued/paused -> running via PUT status).
func TestUpdateNodeStatus_RetryResetsBeforeResponding(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "queued"}); err != nil {
		t.Fatalf("seed queued node: %v", err)
	}
	seedStaleTerminalRun(t, h, chatID)

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusUpdateBodyStatusRunning})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertEventLogAlreadyReset(t, h, chatID)
}

// TestSubscribeDuringNodeStartWindow: a subscriber connecting right after the 200, before the resumed run
// publishes, isn't served the previous run's terminal done.
func TestSubscribeDuringNodeStartWindow(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "queued"}); err != nil {
		t.Fatalf("seed queued node: %v", err)
	}
	seedStaleTerminalRun(t, h, chatID)

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusUpdateBodyStatusRunning})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := subscribe(t, h, chatID, "")
	if strings.Contains(body, staleRunMarker) {
		t.Errorf("subscriber in the start window was served the previous run's stale event/terminal done: %q", body)
	}
}
