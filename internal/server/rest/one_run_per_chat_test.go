package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A send onto a chat with a live run is refused and leaves that run alone; one landing in a finished run's
// tail (its done already reached the client's queued follow-up) waits the tail out and starts.
func TestSendChatMessage_OneRunPerChat(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	cancelled := false
	h.hub.RegisterRun(chatID, "live", func() { cancelled = true })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/responses", strings.NewReader(`{"content":"again"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.SendChatMessage(rec, req, chatID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("send onto a live run: status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !h.hub.CancelResponse(chatID, "live") || !cancelled {
		t.Fatal("the refused send replaced the live run's handle")
	}
	if turns, err := h.store.ListTurns(context.Background(), chatID); err != nil || len(turns) != 0 {
		t.Fatalf("refused send saved %d turns (err %v)", len(turns), err)
	}

	h.hub.EndRun(chatID, "live")
	h.hub.RegisterRun(chatID, "tail", func() {})
	go func() {
		time.Sleep(100 * time.Millisecond)
		h.hub.EndRun(chatID, "tail")
	}()
	postMessage(t, h, chatID, "follow-up")
}
