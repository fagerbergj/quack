package rest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// TestSendChatMessage_DrainingRejects503 proves a shutdown in progress
// refuses new dispatches instead of starting a run nothing will wait for.
func TestSendChatMessage_DrainingRejects503(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	h.hub.BeginDraining()

	body := bytes.NewBufferString(`{"content":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+c.ID+"/messages", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.SendChatMessage(rec, req, c.ID)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestStampRunOutcome_Interrupted proves that once Hub.MarkInterrupted names
// a chat, stampRunOutcome persists RunStatusPaused - the drain paused its
// nodes (#962) and boot resumes them - regardless of what the (empty) turn
// history would otherwise derive.
func TestStampRunOutcome_Interrupted(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.MarkRunActive(ctx, c.ID, "turn-1"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}
	h.hub.MarkInterrupted(c.ID)

	h.stampRunOutcome(ctx, c.ID)

	got, err := h.store.GetChat(ctx, c.ID)
	if err != nil || got == nil {
		t.Fatalf("GetChat: %v, %v", got, err)
	}
	if got.RunStatus != store.RunStatusPaused {
		t.Errorf("RunStatus = %q, want %q", got.RunStatus, store.RunStatusPaused)
	}
}

// TestLiveOrStampedStatus_InterruptedMapsToFailed proves the wire-facing
// summary (ListChats) reports an interrupted chat as failed, same as
// #738's existing ActiveTurnID fallback.
func TestLiveOrStampedStatus_InterruptedMapsToFailed(t *testing.T) {
	h := newTestHandler(t)
	c := store.Chat{ID: "chat-x", RunStatus: store.RunStatusInterrupted}
	status, _ := h.liveOrStampedStatus(c)
	if status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
}
