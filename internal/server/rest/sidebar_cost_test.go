package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/store/storetest"
	"github.com/fagerbergj/quack/internal/vetting"
)

// sessionGetSpy fails and counts any session Get: ListChats must never read session history, which
// profiling showed deserializing whole event histories every poll. Stronger than a bounded query count.
type sessionGetSpy struct {
	session.Service
	gets atomic.Int64
}

func (s *sessionGetSpy) Get(_ context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	s.gets.Add(1)
	return nil, fmt.Errorf("unexpected session.Get(%s/%s): this path must never read session history (#738)", req.UserID, req.SessionID)
}

// newTestHandlerWithSessionSpy is newTestHandler with its session store swapped for a spy
// that fails any Get, so a test can prove a handler path never touches session history.
func newTestHandlerWithSessionSpy(t *testing.T) (*Handler, *sessionGetSpy) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	spy := &sessionGetSpy{Service: st.Sessions}
	st.Sessions = spy
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{}, map[string]model.LLM{}, nil,
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	orch := orchestrator.New(st.Sessions, stubModel{}, func(context.Context) string { return "You are a test duck." }, planner, ex, nil, nil, nil)
	return NewHandler(st, orch, nil, nil, nil, nil, "test", nil, nil, nil, nil), spy
}

// listChatsQueries runs ListChats and returns how many SELECT queries it issued.
func listChatsQueries(t *testing.T, h *Handler) int64 {
	t.Helper()
	queries := storetest.RecordQueries(t, h.store.DB())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec := httptest.NewRecorder()
	h.ListChats(rec, req, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("ListChats status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return int64(len(queries()))
}

// TestListChatsQueryCountBoundedByChatCount: with 100+ chats, ListChats' query count doesn't grow with
// chat count, and it never touches the ADK session store.
func TestListChatsQueryCountBoundedByChatCount(t *testing.T) {
	h, spy := newTestHandlerWithSessionSpy(t)
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
	if got := spy.gets.Load(); got != 0 {
		t.Errorf("ListChats triggered %d session.Get call(s), want 0 - status must never read session history", got)
	}
}

// TestToSummaryReadsStampedOutcome: a normally ended run's status comes from StampRunOutcome's row,
// with no per-chat status read.
func TestToSummaryReadsStampedOutcome(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, c.ID, "t1", ""); err != nil {
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

	queries := storetest.RecordQueries(t, h.store.DB())
	got := h.toSummary(*stamped, 0, false)
	spent := len(queries())

	if got.Status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed (from the stamp)", got.Status)
	}
	if spent != 0 {
		t.Errorf("toSummary issued %d queries reading a stamped chat, want 0", spent)
	}
}

// TestCrashedRunDoesNotStickAtRunning: a run killed after MarkRunActive but before StampRunOutcome must not
// read "running" forever; the read side notices the abandoned marker rather than trust an older stamp.
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
	got := h.toSummary(*c2, 0, false)
	if got.Status == schema.ChatStatusRunning {
		t.Fatal("status = running, want anything but running - a dead run must not read as still live")
	}
	if got.Status != schema.ChatStatusFailed {
		t.Errorf("status = %q, want failed (the crash fallback)", got.Status)
	}
}

// TestListChatsUnchangedIsCheapOnTheWire: an unchanged poll costs much less on the wire via ETag/If-None-Match,
// not a TTL cache (every call still revalidates against the store).
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

// TestListChatsETagVariesWithPage: the ETag covers the page returned, so page 1's ETag
// must not read as unchanged against a page-2 request.
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
