package rest

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/stream"
)

// waitForBody polls the recorder's accumulated body until it contains want, or
// fails the test after a short timeout. httptest.ResponseRecorder's bytes.Buffer
// isn't safe for concurrent read/write, but this package writes+reads only
// strings, in a tight poll loop - fine for a test racing a background writer.
func waitForBody(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(rec.Body.String(), want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %q in body:\n%s", want, rec.Body.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSubscribeLiveTail is issue #282's core scenario: a node is actively
// running (events already published, more still to come) and the request must
// stay open, delivering new events as they land, instead of snapshotting the
// history so far and closing. Crucially, the run here is published exactly as
// the GitHub extension does - through the shared hub, with NOTHING registered
// in the handler's own activeCancels (that map is REST-only; a GitHub-dispatched
// run registers in the extension's separate map) - so this also pins that
// "active" is derived from the shared hub, not that REST-only registry.
func TestSubscribeLiveTail(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1"))
	pub.Publish(stream.NodeStart("n1", "researcher"))

	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.SubscribeChatStream(rec, req, chatID)
		close(done)
	}()

	// Replays what was already published...
	waitForBody(t, rec, "node_start")

	// ...then, while still connected, a subsequent live event must arrive
	// without a reload - the request must NOT have already completed.
	select {
	case <-done:
		t.Fatal("stream completed instead of staying open for the live tail")
	default:
	}
	pub.Publish(stream.NodeDone("n1", stream.NodeDoneData{}))
	waitForBody(t, rec, "node_done")

	// The run ending closes the stream - mirrors what every driver of a run
	// (REST handler, GitHub webhook dispatcher) does: publish Done, then close
	// the hub topic so it stops accepting a next run's events as this one's.
	pub.Publish(stream.Done())
	h.hub.Close(chatID)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after the run's done event")
	}
	cancel()
}

// TestSubscribeIdleSnapshotsAndCloses is the non-regression counterpart: a
// finished (or never-started) chat must still snapshot-and-close promptly -
// the fix for #282 must not turn every stream into a hanging connection.
func TestSubscribeIdleSnapshotsAndCloses(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1"))
	pub.Publish(stream.NodeStart("n1", "researcher"))
	pub.Publish(stream.NodeDone("n1", stream.NodeDoneData{}))
	pub.Publish(stream.Done())
	h.hub.Close(chatID)

	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.SubscribeChatStream(rec, req, chatID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a finished run's stream should close promptly, not hang")
	}
	for _, want := range []string{"node_start", "node_done", "event: done"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("snapshot missing %q in:\n%s", want, rec.Body.String())
		}
	}
}

// TestSubscribeLiveReconnectByLastEventID: a reconnect mid-run (Last-Event-ID
// set) must resume past what the client already saw and pick up the live
// tail without duplicating or dropping events - not just on the cold/durable
// path (TestSubscribeColdReplay covers that), but on the warm hub path too.
func TestSubscribeLiveReconnectByLastEventID(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1")) // seq 1
	pub.Publish(stream.NodeStart("n1", "rs")) // seq 2

	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.SubscribeChatStream(rec, req, chatID)
		close(done)
	}()

	waitForBody(t, rec, "id: 2")
	if strings.Contains(rec.Body.String(), "id: 1\n") {
		t.Errorf("reconnect from seq 1 must not resend seq 1 (dup); body:\n%s", rec.Body.String())
	}

	pub.Publish(stream.NodeDone("n1", stream.NodeDoneData{})) // seq 3, live
	waitForBody(t, rec, "id: 3")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
}
