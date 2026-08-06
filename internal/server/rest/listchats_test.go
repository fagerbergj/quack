package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

func getListChats(t *testing.T, h *Handler, params schema.ListChatsParams) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec := httptest.NewRecorder()
	h.ListChats(rec, req, params)
	return rec
}

func decodeChatList(t *testing.T, rec *httptest.ResponseRecorder) schema.ChatList {
	t.Helper()
	var out schema.ChatList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode ChatList: %v", err)
	}
	return out
}

// TestListChats_NoParams: a request with no parameters still works (issue
// #736 test case 4) - the current SPA and CLI send none.
func TestListChats_NoParams(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := h.store.CreateChat(ctx, ""); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
	}

	rec := getListChats(t, h, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeChatList(t, rec)
	if len(out.Data) != 3 {
		t.Fatalf("len(data) = %d, want 3", len(out.Data))
	}
	if out.NextCursor != nil {
		t.Fatalf("next_cursor = %q, want absent (fewer chats than the default page size)", *out.NextCursor)
	}
}

// TestListChats_LimitAndCursorPageThrough proves the query params reach the
// store and a cursor from one response fetches the next page.
func TestListChats_LimitAndCursorPageThrough(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	ids := map[string]bool{}
	for i := 0; i < 5; i++ {
		c, err := h.store.CreateChat(ctx, "")
		if err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
		ids[c.ID] = true
	}

	limit := 2
	rec := getListChats(t, h, schema.ListChatsParams{Limit: &limit})
	page1 := decodeChatList(t, rec)
	if len(page1.Data) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Data))
	}
	if page1.NextCursor == nil {
		t.Fatal("page1.NextCursor is nil, want a next-page cursor (5 chats > limit 2)")
	}

	rec = getListChats(t, h, schema.ListChatsParams{Limit: &limit, Cursor: page1.NextCursor})
	page2 := decodeChatList(t, rec)
	if len(page2.Data) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2.Data))
	}

	seen := map[string]bool{}
	for _, c := range append(page1.Data, page2.Data...) {
		if seen[c.Id] {
			t.Fatalf("chat %s returned twice across pages", c.Id)
		}
		seen[c.Id] = true
		if !ids[c.Id] {
			t.Fatalf("unexpected chat id %s", c.Id)
		}
	}
}

// TestListChats_InvalidCursor400s: a cursor the store can't decode is a
// client error, not a 500.
func TestListChats_InvalidCursor400(t *testing.T) {
	h := newTestHandler(t)
	bad := "not-a-valid-cursor!!"
	rec := getListChats(t, h, schema.ListChatsParams{Cursor: &bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
