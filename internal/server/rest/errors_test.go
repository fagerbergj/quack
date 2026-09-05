package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
)

// failingLedgerStore's List always errors (never ErrNotExist) - the only
// deterministic way in this suite to force a 500 out of a handler without
// touching the store's DB connection directly.
type failingLedgerStore struct{}

func (failingLedgerStore) List(context.Context) ([]ledger.SessionRef, error) {
	return nil, errors.New("ledger: unreachable")
}
func (failingLedgerStore) Delete(context.Context, string) error { return nil }
func (failingLedgerStore) AppendIntent(context.Context, ledger.Entry) (int64, error) {
	return 0, errors.New("ledger: unreachable")
}
func (failingLedgerStore) ReadEntries(context.Context, string, int64) ([]ledger.Entry, error) {
	return nil, errors.New("ledger: unreachable")
}

// TestErrorResponseShape is a table-driven check that every representative
// 4xx/5xx path emits the same JSON schema.ErrorResponse shape (a non-empty
// "error" field) with application/json content-type, never http.Error's
// plain text - the contract finding-1 fixed openapi.yaml to declare.
func TestErrorResponseShape(t *testing.T) {
	cases := []struct {
		name       string
		wantStatus int
		do         func(t *testing.T) *httptest.ResponseRecorder
	}{
		{
			name:       "400 ListChats explicitly empty status",
			wantStatus: http.StatusBadRequest,
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newTestHandler(t)
				status := []schema.ListChatsParamsStatus{}
				return getListChats(t, h, schema.ListChatsParams{Status: &status})
			},
		},
		{
			name:       "404 GetChat unknown id",
			wantStatus: http.StatusNotFound,
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newTestHandler(t)
				rec := httptest.NewRecorder()
				h.GetChat(rec, httptest.NewRequest(http.MethodGet, "/api/v1/chats/no-such-chat", nil), "no-such-chat")
				return rec
			},
		},
		{
			name:       "409 UpdateNodeStatus illegal transition",
			wantStatus: http.StatusConflict,
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newTestHandler(t)
				chatID, planID, nodeID := "c1", "p1", "n1"
				seedPlan(t, h, chatID, planID, nodeID)
				if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "done"}); err != nil {
					t.Fatalf("seed done node: %v", err)
				}
				// done -> needs_input is illegal (done only legally re-queues via retry).
				return putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusNeedsInput})
			},
		},
		{
			name:       "500 ListRecordings ledger unreachable",
			wantStatus: http.StatusInternalServerError,
			do: func(t *testing.T) *httptest.ResponseRecorder {
				h := newTestHandler(t)
				h.ledgerStore = failingLedgerStore{}
				rec := httptest.NewRecorder()
				h.ListRecordings(rec, httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil))
				return rec
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.do(t)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			var out schema.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode ErrorResponse: %v (body=%s)", err, rec.Body.String())
			}
			if out.Error == "" {
				t.Fatalf("error field is empty; body=%s", rec.Body.String())
			}
		})
	}
}
