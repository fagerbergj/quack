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

// TestGetChat_UsageAggregatesTurnsAndNodes proves GetChat's usage field sums
// both the SQL-summable places a chat spends tokens - the plain-reply turn
// row and its DAG nodes - not just one of them.
func TestGetChat_UsageAggregatesTurnsAndNodes(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	c, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, c.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := h.store.SetTurnUsage(ctx, c.ID, "t1", "gpt-oss-120b", store.TurnUsage{
		PromptTokens: 100, CompletionTokens: 20, ReasoningTokens: 5, TotalTokens: 125, CachedTokens: 30,
	}); err != nil {
		t.Fatalf("SetTurnUsage: %v", err)
	}
	if err := h.store.SaveTurn(ctx, c.ID, "t2"); err != nil {
		t.Fatalf("SaveTurn t2: %v", err)
	}
	if err := h.store.SaveDagPlan(ctx, c.ID, "p1", "t2", `{"nodes":[]}`); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := h.store.UpsertDagNode(ctx, store.DagNode{
		NodeID: "n1", PlanID: "p1", Status: "done",
		PromptTokens: 200, CompletionTokens: 40, ReasoningTokens: 10, TotalTokens: 250, CachedTokens: 60,
	}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+c.ID, nil)
	rec := httptest.NewRecorder()
	h.GetChat(rec, req, c.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail schema.ChatDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := detail.Usage
	if intVal(got.InputTokens) != 300 || intVal(got.OutputTokens) != 60 ||
		intVal(got.ReasoningTokens) != 15 || intVal(got.CachedTokens) != 90 || intVal(got.TotalTokens) != 375 {
		t.Fatalf("ChatDetail.Usage = %+v, want turn(100/20/5/125/30) + node(200/40/10/250/60)", got)
	}
	if intVal(detail.TotalTokens) != 375 {
		t.Fatalf("ChatDetail.TotalTokens = %v, want 375", detail.TotalTokens)
	}
}

// TestListChats_TotalTokens proves the sidebar list carries the compact
// total_tokens - a chat with spend shows it, a fresh chat doesn't (omitted,
// not a spurious 0).
func TestListChats_TotalTokens(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()

	spent, err := h.store.CreateChat(ctx, "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, spent.ID, "t1"); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	if err := h.store.SetTurnUsage(ctx, spent.ID, "t1", "gpt-oss-120b", store.TurnUsage{TotalTokens: 42}); err != nil {
		t.Fatalf("SetTurnUsage: %v", err)
	}
	if _, err := h.store.CreateChat(ctx, ""); err != nil {
		t.Fatalf("CreateChat fresh: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil)
	rec := httptest.NewRecorder()
	h.ListChats(rec, req, schema.ListChatsParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list schema.ChatList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := make(map[string]schema.ChatSummary, len(list.Data))
	for _, c := range list.Data {
		byID[c.Id] = c
	}
	if intVal(byID[spent.ID].TotalTokens) != 42 {
		t.Fatalf("spent chat total_tokens = %v, want 42", byID[spent.ID].TotalTokens)
	}
	for id, c := range byID {
		if id != spent.ID && c.TotalTokens != nil {
			t.Fatalf("fresh chat %s total_tokens = %v, want omitted", id, *c.TotalTokens)
		}
	}
}

func intVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
