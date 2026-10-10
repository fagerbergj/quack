package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

// TestSendChatMessage_UnknownChat404: an unknown chat_id 404s before the SSE stream opens,
// never as an in-stream error event.
func TestSendChatMessage_UnknownChat404(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/no-such-chat/responses", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.SendChatMessage(rec, req, "no-such-chat")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (never an SSE stream)", ct)
	}
	var out schema.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Error == "" {
		t.Fatalf("body is not a populated ErrorResponse: %v (body=%s)", err, rec.Body.String())
	}
}

// TestSubscribeChatStream_UnknownChat404 is the same check for the standalone subscribe endpoint.
func TestSubscribeChatStream_UnknownChat404(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/no-such-chat/stream", nil)
	rec := httptest.NewRecorder()
	h.SubscribeChatStream(rec, req, "no-such-chat")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (never an SSE stream)", ct)
	}
	var out schema.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Error == "" {
		t.Fatalf("body is not a populated ErrorResponse: %v (body=%s)", err, rec.Body.String())
	}
}
