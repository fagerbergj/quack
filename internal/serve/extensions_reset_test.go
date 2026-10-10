package serve

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// staleDispatchMarker tags a finished dispatch's leftover event, to tell a re-dispatch's own events
// apart from a stale leak.
const staleDispatchMarker = "STALE-PREVIOUS-DISPATCH-MARKER"

// TestExtDispatch_ResetsBeforeAck: the durable event log is reset before the dispatch ack, so a subscriber
// racing a re-dispatch can't read the previous dispatch's stale terminal event.
func TestExtDispatch_ResetsBeforeAck(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	eventLog := runlog.NewEventLog(st)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, eventLog, &extHolder, nil, artifacts)

	const localID = "reset-before-ack"
	chatID := "ext:noop:" + localID
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	js, err := runlog.MarshalEvent(stream.Errorf(staleDispatchMarker))
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := st.InsertChatEvent(context.Background(), store.ChatEvent{ChatID: chatID, Seq: 999999, Event: js}); err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}

	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	evs, err := eventLog.LoadEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Event, staleDispatchMarker) {
			t.Fatalf("stale marker event was still present immediately after the re-dispatch acked; the "+
				"reset must run synchronously before Dispatch returns, not from inside the run's own "+
				"spawned goroutine: %q", ev.Event)
		}
	}
}
