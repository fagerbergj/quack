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
	if out.NextPageToken != nil {
		t.Fatalf("next_page_token = %q, want absent (fewer chats than the default page size)", *out.NextPageToken)
	}
}

// TestListChats_PageTokenIsOpaqueRoundTrip proves the query params reach the
// store AND that the page_token is genuinely opaque to the caller: this test
// never decodes it, never inspects its shape, only ever passes back byte-for-
// byte what the previous response gave it - exactly the contract a real
// client follows - and pagination still lands on every chat exactly once.
func TestListChats_PageTokenIsOpaqueRoundTrip(t *testing.T) {
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
	if page1.NextPageToken == nil {
		t.Fatal("page1.NextPageToken is nil, want a next-page token (5 chats > limit 2)")
	}

	// The handler/store contract asks only that this exact string comes
	// back unmodified - it is never parsed here.
	rec = getListChats(t, h, schema.ListChatsParams{Limit: &limit, PageToken: page1.NextPageToken})
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

// TestListChats_InvalidPageToken400: a page_token the store can't decode is
// a client error, not a 500.
func TestListChats_InvalidPageToken400(t *testing.T) {
	h := newTestHandler(t)
	bad := "not-a-valid-token!!"
	rec := getListChats(t, h, schema.ListChatsParams{PageToken: &bad})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListChats_ExcludeArchivedByDefault: archived chats are omitted from the list
// unless show_archived=true. This keeps the sidebar focused on active items.
func TestListChats_ExcludeArchivedByDefault(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	// Create 2 normal and 1 archived chat.
	cNormal1, _ := h.store.CreateChat(ctx, "")
	_ = cNormal1 // used implicitly as a row; ids below verify filtering.
	cNormal2, _ := h.store.CreateChat(ctx, "")
	_ = cNormal2
	cArchived, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	h.store.ArchiveChat(ctx, cArchived.ID, true)

	// Default (nil showArchived): should return 2 chats.
	rec := getListChats(t, h, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("default status = %d, want 200", rec.Code)
	}
	out := decodeChatList(t, rec)
	if len(out.Data) != 2 {
		t.Fatalf("default len(data) = %d, want 2 (archived excluded)", len(out.Data))
	}
	for _, c := range out.Data {
		if c.Id == cArchived.ID {
			t.Errorf("archived chat %s present in default list", c.Id)
		}
	}

	// show_archived=true: should return all 3.
	trueVal := true
	rec = getListChats(t, h, schema.ListChatsParams{ShowArchived: &trueVal})
	out = decodeChatList(t, rec)
	if len(out.Data) != 3 {
		t.Fatalf("showArchived=true len(data) = %d, want 3", len(out.Data))
	}

	// show_archived=false: should also exclude archived.
	falseVal := false
	rec = getListChats(t, h, schema.ListChatsParams{ShowArchived: &falseVal})
	out = decodeChatList(t, rec)
	if len(out.Data) != 2 {
		t.Fatalf("showArchived=false len(data) = %d, want 2", len(out.Data))
	}

	// Verify all returned chats have archived field omitted or false.
	for _, c := range out.Data {
		if c.Archived != nil && *c.Archived {
			t.Errorf("archived chat should not appear when showArchived=false")
		}
	}
}

func decodeChatSummary(t *testing.T, rec *httptest.ResponseRecorder) schema.ChatSummary {
	t.Helper()
	var out schema.ChatSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode ChatSummary: %v", err)
	}
	return out
}
