package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// TestChatStatus_ListAndDetailAgree: GET /chats and GET /chats/{id} read one status rule,
// so a chat never shows failed in the list and idle in the detail (or the reverse).
func TestChatStatus_ListAndDetailAgree(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *Handler, chatID string)
		want  schema.ChatStatus
	}{
		{"idle", func(*testing.T, *Handler, string) {}, schema.ChatStatusIdle},
		{"failed by the orphan scan", func(t *testing.T, h *Handler, chatID string) {
			if err := h.store.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := h.store.ScanOrphanedRuns(ctx); err != nil {
				t.Fatal(err)
			}
		}, schema.ChatStatusFailed},
		{"running on a node row", func(t *testing.T, h *Handler, chatID string) {
			seedPlan(t, h, chatID, "p-"+chatID, "n1")
			if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: "n1", PlanID: "p-" + chatID, Status: "running"}); err != nil {
				t.Fatal(err)
			}
		}, schema.ChatStatusRunning},
		{"running on the hub", func(_ *testing.T, h *Handler, chatID string) {
			h.hub.Publish(chatID, 1, stream.ResponseCreated("turn-1"))
		}, schema.ChatStatusRunning},
		{"dispatched, not yet publishing", func(t *testing.T, h *Handler, chatID string) {
			h.hub.RegisterRun(chatID, "turn-1", func() {})
			if err := h.store.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
				t.Fatal(err)
			}
		}, schema.ChatStatusRunning},
		{"paused for boot", func(t *testing.T, h *Handler, chatID string) {
			if err := h.store.StampRunOutcome(ctx, chatID, store.RunStatusPaused, ""); err != nil {
				t.Fatal(err)
			}
		}, schema.ChatStatusIdle},
		{"needs input", func(t *testing.T, h *Handler, chatID string) {
			if err := h.store.StampRunOutcome(ctx, chatID, store.RunStatusNeedsInput, "which?"); err != nil {
				t.Fatal(err)
			}
		}, schema.ChatStatusNeedsInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t)
			chatID := mustCreateChat(t, h)
			tc.setup(t, h, chatID)

			rec := httptest.NewRecorder()
			h.GetChat(rec, httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID, nil), chatID)
			var detail schema.ChatDetail
			if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
				t.Fatalf("GetChat: %d %s", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			h.ListChats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil), schema.ListChatsParams{})
			var list schema.ChatList
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Data) != 1 {
				t.Fatalf("ListChats: %d %s", rec.Code, rec.Body.String())
			}
			if detail.Status != tc.want || list.Data[0].Status != tc.want {
				t.Errorf("detail = %q, list = %q, want both %q", detail.Status, list.Data[0].Status, tc.want)
			}
		})
	}
}

// TestChatStatus_LiveRunSkipsNodeQuery: a chat the hub already reports running needs no
// running-node lookup, in the detail or the list.
func TestChatStatus_LiveRunSkipsNodeQuery(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)
	h.hub.Publish(chatID, 1, stream.ResponseCreated("turn-1"))
	h.store.EnableQueryRecording()
	h.GetChat(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID, nil), chatID)
	h.ListChats(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil), schema.ListChatsParams{})
	for _, q := range h.store.RecordedQuerySQL() {
		if strings.Contains(q, "JOIN dag_plans") && strings.Contains(q, "dag_nodes.status") {
			t.Errorf("ran the running-node query for a live chat: %s", q)
		}
	}
}
