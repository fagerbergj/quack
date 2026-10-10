package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/stream"
)

// syncRecorder guards httptest.ResponseRecorder, whose buffer isn't safe for the handler-writes/test-polls
// race. Not embedded: promoted Result/Code/Header would bypass mu.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{rec: httptest.NewRecorder()}
}

func (s *syncRecorder) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Header()
}

func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(p)
}

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

func (s *syncRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

// waitForBody polls the recorder's accumulated body until it contains want, or
// fails the test after a short timeout.
func waitForBody(t *testing.T, rec *syncRecorder, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(rec.body(), want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %q in body:\n%s", want, rec.body())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSubscribeLiveTail: a running node's stream stays open and delivers new events as they land. The run
// is published through the shared hub only, as the GitHub extension does, so "active" must come from the hub.
func TestSubscribeLiveTail(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(context.Background(), h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1"))
	pub.Publish(stream.NodeStart("n1", "researcher"))

	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newSyncRecorder()
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

	// Every run driver publishes Done and then closes the hub topic, so it can't accept
	// a next run's events as this one's.
	pub.Publish(stream.Done())
	h.hub.Close(chatID)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after the run's done event")
	}
	cancel()
}

// TestSubscribeIdleSnapshotsAndCloses: a finished or never-started chat still snapshots and closes
// promptly rather than hanging.
func TestSubscribeIdleSnapshotsAndCloses(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(context.Background(), h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1"))
	pub.Publish(stream.NodeStart("n1", "researcher"))
	pub.Publish(stream.NodeDone("n1", stream.NodeDoneData{}))
	pub.Publish(stream.Done())
	h.eventLog.Flush() // Close frees the hub buffer; must wait for the durable write first (see startRun).
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

// TestSubscribeLiveReconnectByLastEventID: a mid-run reconnect with Last-Event-ID resumes on the warm hub
// path without duplicating or dropping events.
func TestSubscribeLiveReconnectByLastEventID(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(context.Background(), h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1")) // seq 1
	pub.Publish(stream.NodeStart("n1", "rs")) // seq 2

	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		h.SubscribeChatStream(rec, req, chatID)
		close(done)
	}()

	waitForBody(t, rec, "id: 2")
	if strings.Contains(rec.body(), "id: 1\n") {
		t.Errorf("reconnect from seq 1 must not resend seq 1 (dup); body:\n%s", rec.body())
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

// TestSubscribeCloseRacesActiveRead: a Hub.Close between the Active read and Subscribe leaves a nil live
// channel; the buffered replay must still be written. Asserts body content, not just that the call returned.
func TestSubscribeCloseRacesActiveRead(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	pub := runlog.NewPublisher(context.Background(), h.hub, h.eventLog, chatID)
	pub.Publish(stream.ResponseCreated("t1"))
	pub.Publish(stream.NodeDone("n1", stream.NodeDoneData{}))
	pub.Publish(stream.Done())
	h.eventLog.Flush() // Close frees the hub buffer; must wait for the durable write first.

	restore := subscribeRaceHook
	subscribeRaceHook = func() { h.hub.Close(chatID) }
	defer func() { subscribeRaceHook = restore }()

	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	rec := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		h.SubscribeChatStream(rec, req, chatID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// The buggy path blocks forever on the nil channel; the content check below
		// is what tells the two apart.
	}
	if !strings.Contains(rec.body(), "node_done") {
		t.Fatalf("Hub.Close racing the Active read dropped the buffered replay; body:\n%q", rec.body())
	}
}
