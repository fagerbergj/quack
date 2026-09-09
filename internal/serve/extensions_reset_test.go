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

// staleDispatchMarker tags a finished dispatch's leftover durable event so a
// test can tell "the re-dispatch already legitimately wrote its own events"
// apart from "the previous dispatch's stale events leaked through".
const staleDispatchMarker = "STALE-PREVIOUS-DISPATCH-MARKER"

// TestExtDispatch_ResetsBeforeAck pins finding 5 for the extension dispatch
// path: newExtDispatch used to reset the durable event log only from inside
// driveExtensionRun's own spawned goroutine, so a subscriber racing a
// re-dispatch's ack (e.g. a nudge/retry, quack-extensions#47) could read the
// previous dispatch's stale terminal event straight off the durable table.
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
