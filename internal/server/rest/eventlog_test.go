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

// TestSubscribeColdReplay drives the restart path: no hub topic, no active run, so
// SubscribeChatStream replays the run from the durable log, emitting each event's
// seq as the SSE id. A Last-Event-ID resumes from the next event.
func TestSubscribeColdReplay(t *testing.T) {
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	h := &Handler{store: st, hub: stream.NewHub(), eventLog: runlog.NewEventLog(st)}
	ctx := context.Background()
	for i, ev := range []stream.SSEEvent{
		stream.NodeStart("n1", "researcher"),
		stream.NodeDone("n1", stream.NodeDoneData{}),
		stream.Done(),
	} {
		js, _ := runlog.MarshalEvent(ev)
		if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: "c", Seq: int64(i + 1), Event: js}); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Full cold replay: all three events, each carrying its id.
	body := subscribe(t, h, "c", "")
	for _, want := range []string{"id: 1", "id: 2", "id: 3", "event: node_start", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("cold replay missing %q in:\n%s", want, body)
		}
	}

	// Resume from seq 2 → only event 3 (the Done).
	body = subscribe(t, h, "c", "2")
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
