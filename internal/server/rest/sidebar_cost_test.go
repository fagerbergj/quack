package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// listChatsQueries runs ListChats and returns how many SELECT queries it issued.
func listChatsQueries(t *testing.T, h *Handler) int64 {
	t.Helper()
	before := h.store.QueryCount()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec := httptest.NewRecorder()
	h.ListChats(rec, req, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("ListChats status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return h.store.QueryCount() - before
}

// TestListChatsQueryCountBoundedByChatCount is #738 test 1: with 100+ chats stored,
// ListChats issues a bounded number of queries that does not grow with chat count -
// the N+1 the ponytail marker on the old ListChats named (one toSummary DB read per row).
func TestListChatsQueryCountBoundedByChatCount(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	for range 5 {
		if _, err := h.store.CreateChat(ctx, ""); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
	}
	small := listChatsQueries(t, h)

	for range 150 {
		if _, err := h.store.CreateChat(ctx, ""); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
	}
	large := listChatsQueries(t, h)

	if large != small {
		t.Errorf("queries for 155 chats = %d, want %d (same as 5 chats - status must not be a per-chat read)", large, small)
	}
	if large > 2 {
		t.Errorf("queries for 155 chats = %d, want a small constant (1 chats read)", large)
	}
}

// TestToSummaryReadsStampedOutcome is #738 test 2: a chat whose run ends normally shows
// its final status from the row StampRunOutcome stamped, with no per-chat status read.
func TestToSummaryReadsStampedOutcome(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, c.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := h.store.SaveDagPlan(ctx, c.ID, "p1", "t1", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: "n1", PlanID: "p1", Status: "failed", Error: "boom"}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	// Simulates the run-end call every real driver (runChat/retryNodeAsync/github
	// dispatch) makes; it resolves the same failed status GetChat derives live.
	h.stampRunOutcome(ctx, c.ID)
	stamped, err := h.store.GetChat(ctx, c.ID)
	if err != nil || stamped == nil {
		t.Fatalf("GetChat after stamp: %+v, %v", stamped, err)
	}

	before := h.store.QueryCount()
	got := h.toSummary(*stamped)
	spent := h.store.QueryCount() - before

	if got.Status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed (from the stamp)", got.Status)
	}
	if spent != 0 {
		t.Errorf("toSummary issued %d queries reading a stamped chat, want 0", spent)
	}
}

// TestCrashedRunDoesNotStickAtRunning is #738 test 3: a run killed mid-flight (MarkRunActive
// ran, StampRunOutcome never did - simulating process death before the deferred stamp)
// must not leave the chat reading "running" forever. ListChats never persists "running" in
// the first place (it's hub.Active, live and per-process - see stream.NewHub's ponytail on
// single-instance scope) so a fresh process trivially reports not-running; the guard this
// pins is the READ side noticing the abandoned marker instead of trusting a stale idle/
// needs_input stamp left over from a run before the one that crashed.
func TestCrashedRunDoesNotStickAtRunning(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	// A prior run completed cleanly and stamped idle.
	h.stampRunOutcome(ctx, c.ID)
	// A second run starts (MarkRunActive) and never finishes - no StampRunOutcome, no
	// hub.Active (this process, like a fresh one after a restart, has no live record of it).
	if err := h.store.MarkRunActive(ctx, c.ID, "t2"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}

	c2, err := h.store.GetChat(ctx, c.ID)
	if err != nil || c2 == nil {
		t.Fatalf("GetChat: %+v, %v", c2, err)
	}
	got := h.toSummary(*c2)
	if got.Status == schema.ChatStatusRunning {
		t.Fatal("status = running, want anything but running - a dead run must not read as still live")
	}
	if got.Status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed (the crash fallback)", got.Status)
	}
}

// TestListChatsUnchangedIsCheapOnTheWire is #738 test 5: a poll that finds nothing changed
// costs materially less on the wire than one that finds a change - a conditional GET via
// ETag/If-None-Match, not a TTL cache (every call still reaches the store and revalidates).
func TestListChatsUnchangedIsCheapOnTheWire(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	if _, err := h.store.CreateChat(ctx, ""); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec := httptest.NewRecorder()
	h.ListChats(rec, req, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("first ListChats status = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response carries no ETag")
	}
	changedLen := rec.Body.Len()

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.ListChats(rec2, req2, schema.ListChatsParams{})
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("second ListChats (unchanged) status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() >= changedLen {
		t.Errorf("unchanged response body = %d bytes, want materially less than the changed one (%d)", rec2.Body.Len(), changedLen)
	}

	// A real change (new chat) must bust the ETag and cost a full body again.
	if _, err := h.store.CreateChat(ctx, ""); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	req3.Header.Set("If-None-Match", etag)
	rec3 := httptest.NewRecorder()
	h.ListChats(rec3, req3, schema.ListChatsParams{})
	if rec3.Code != http.StatusOK {
		t.Errorf("ListChats after a real change status = %d, want 200 (stale ETag)", rec3.Code)
	}
}

// TestListChatsETagVariesWithPage is #738 requirement 3: the ETag is computed over the
// page actually returned, so page 2's ETag must differ from page 1's, and replaying
// page 1's ETag against a page-2 request must not read as unchanged (a naive ETag over
// just "the chats table changed" would wrongly 304 page 2 with page 1's stale ETag).
func TestListChatsETagVariesWithPage(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	for range 5 {
		if _, err := h.store.CreateChat(ctx, ""); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
	}

	limit := 2
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec1 := httptest.NewRecorder()
	h.ListChats(rec1, req1, schema.ListChatsParams{Limit: &limit})
	if rec1.Code != http.StatusOK {
		t.Fatalf("page1 status = %d", rec1.Code)
	}
	etag1 := rec1.Header().Get("ETag")
	var page1 schema.ChatList
	if err := json.Unmarshal(rec1.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if page1.NextPageToken == nil {
		t.Fatal("page1.NextPageToken is nil, want a next-page token (5 chats > limit 2)")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec2 := httptest.NewRecorder()
	h.ListChats(rec2, req2, schema.ListChatsParams{Limit: &limit, PageToken: page1.NextPageToken})
	if rec2.Code != http.StatusOK {
		t.Fatalf("page2 status = %d", rec2.Code)
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == "" {
		t.Fatal("page2 response carries no ETag")
	}
	if etag1 == etag2 {
		t.Fatal("page1 and page2 ETags are equal, want distinct ETags for distinct pages")
	}

	// Replaying page1's ETag against the page2 request must not 304 - that would serve
	// page1's stale content as if it were page2's.
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	req3.Header.Set("If-None-Match", etag1)
	rec3 := httptest.NewRecorder()
	h.ListChats(rec3, req3, schema.ListChatsParams{Limit: &limit, PageToken: page1.NextPageToken})
	if rec3.Code != http.StatusOK {
		t.Fatalf("page2 request with page1's ETag status = %d, want 200 (must not be served as unchanged)", rec3.Code)
	}
}
