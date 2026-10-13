package rest

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// marshalEvent → unmarshalEvent must round-trip to the identical wire bytes the
// live stream sends (Data stays raw JSON), so a replayed event is indistinguishable.
func TestEventCodecRoundTrip(t *testing.T) {
	orig := stream.NodeStart("n1", "researcher")
	js, err := runlog.MarshalEvent(orig)
	if err != nil {
		t.Fatalf("marshalEvent: %v", err)
	}
	got, err := runlog.UnmarshalEvent(js)
	if err != nil {
		t.Fatalf("unmarshalEvent: %v", err)
	}
	if got.Name != orig.Name {
		t.Errorf("name = %q, want %q", got.Name, orig.Name)
	}
	want, _ := json.Marshal(orig.Data)
	have, _ := json.Marshal(got.Data)
	if string(have) != string(want) {
		t.Errorf("data = %s, want %s", have, want)
	}
}

// TestSubscribeColdReplay: with no hub topic or active run, SubscribeChatStream replays the durable log
// with each event's seq as the SSE id; Last-Event-ID resumes from the next event.
func TestSubscribeColdReplay(t *testing.T) {
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	h := &Handler{store: st, hub: stream.NewHub(), eventLog: runlog.NewEventLog(st)}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	chatID := c.ID
	for i, ev := range []stream.SSEEvent{
		stream.NodeStart("n1", "researcher"),
		stream.NodeDone("n1", stream.NodeDoneData{}),
		stream.Done(),
	} {
		js, _ := runlog.MarshalEvent(ev)
		if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: int64(i + 1), Event: js}); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Full cold replay: all three events, each carrying its id.
	body := subscribe(t, h, chatID, "")
	for _, want := range []string{"id: 1", "id: 2", "id: 3", "event: node_start", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("cold replay missing %q in:\n%s", want, body)
		}
	}

	// Resume from seq 2 → only event 3 (the Done).
	body = subscribe(t, h, chatID, "2")
	if strings.Contains(body, "id: 1") || strings.Contains(body, "id: 2") || !strings.Contains(body, "id: 3") {
		t.Errorf("resume from seq 2 should send only id 3, got:\n%s", body)
	}
}

// subscribe runs SubscribeChatStream against a recorder and returns the body. The
// cold path returns promptly (no live tail), so the recorder doesn't block.
func subscribe(t *testing.T, h *Handler, chatID, lastEventID string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/chats/"+chatID+"/stream", nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	w := httptest.NewRecorder()
	h.SubscribeChatStream(w, req, chatID)
	return w.Body.String()
}

// A follower holding events a hard kill never persisted gets boot's settle failure past its own
// Last-Event-ID, instead of a log that can never reach it; a chat that didn't fail gets nothing extra.
func TestSubscribeColdEndsAFollowerAheadOfTheLog(t *testing.T) {
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	h := &Handler{store: st, hub: stream.NewHub(), eventLog: runlog.NewEventLog(st)}
	ctx := context.Background()
	c, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	for i, ev := range []stream.SSEEvent{stream.Errorf(stream.RestartNotResumed), stream.Done()} {
		js, _ := runlog.MarshalEvent(ev)
		if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: c.ID, Seq: int64(i + 1), Event: js}); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	if body := subscribe(t, h, c.ID, "7"); body != "" {
		t.Fatalf("a chat not stamped failed got %q, want nothing", body)
	}
	if err := st.StampRunOutcome(ctx, c.ID, store.RunStatusFailed, ""); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	body := subscribe(t, h, c.ID, "7")
	for _, want := range []string{"id: 8\nevent: error", stream.RestartNotResumed, "id: 9\nevent: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("follower ahead of the log missing %q in:\n%s", want, body)
		}
	}
	if body := subscribe(t, h, c.ID, "2"); body != "" {
		t.Errorf("a caught-up follower got %q, want nothing", body)
	}
}
