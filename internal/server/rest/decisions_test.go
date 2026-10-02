package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/schema"
)

func TestListDecisions(t *testing.T) {
	h := newTestHandler(t)
	h.quackVersion = "9.9.9"
	st := ledgertest.NewMemStore()
	h.ledgerStore = st
	now := time.Now()
	add := func(chat, point string, at time.Time) {
		p, _ := json.Marshal(ledger.DecisionPayload{Point: point, Handler: "clef", Top: "a", Baseline: "a", State: json.RawMessage(`{"x":1}`)})
		if _, err := st.AppendIntent(context.Background(), ledger.Entry{ChatID: chat, NodeID: "n", Kind: ledger.KindDecision, At: at, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	add("c1", "p1", now.Add(-48*time.Hour))
	add("c1", "p2", now)
	add("c2", "p1", now)
	if _, err := st.AppendIntent(context.Background(), ledger.Entry{ChatID: "c1", Kind: ledger.KindLLMCall, At: now}); err != nil {
		t.Fatal(err)
	}

	call := func(params schema.ListDecisionsParams) schema.DecisionList {
		rec := httptest.NewRecorder()
		h.ListDecisions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/decisions", nil), params)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var out schema.DecisionList
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := call(schema.ListDecisionsParams{}); len(out.Data) != 3 || out.QuackVersion != "9.9.9" || out.Data[0].State != nil {
		t.Errorf("all = %+v", out)
	}
	since := now.Add(-time.Hour)
	if out := call(schema.ListDecisionsParams{Since: &since}); len(out.Data) != 2 {
		t.Errorf("since = %d, want 2", len(out.Data))
	}
	p1, chats, ws := "p1", []string{"c2"}, true
	out := call(schema.ListDecisionsParams{Point: &p1, Chat: &chats, WithState: &ws})
	if len(out.Data) != 1 || out.Data[0].ChatId != "c2" || out.Data[0].State == nil {
		t.Errorf("point+chat+state = %+v", out.Data)
	}
}

func TestListDecisionsNoStore(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler(t).ListDecisions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/decisions", nil), schema.ListDecisionsParams{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestListDecisionsOrderSkippedLimit(t *testing.T) {
	h := newTestHandler(t)
	st := ledgertest.NewMemStore()
	h.ledgerStore = st
	now := time.Now()
	add := func(chat string, at time.Time, payload []byte) {
		if _, err := st.AppendIntent(context.Background(), ledger.Entry{ChatID: chat, NodeID: "n", Kind: ledger.KindDecision, At: at, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := json.Marshal(ledger.DecisionPayload{Point: "p1", Handler: "clef"})
	// Append order contradicts time order, as PG's chat_id,seq read order does.
	add("c2", now, p)
	add("c1", now.Add(-2*time.Hour), p)
	add("c3", now.Add(-time.Hour), p)
	add("c4", now.Add(-time.Minute), []byte(`{"point":`))

	call := func(params schema.ListDecisionsParams) schema.DecisionList {
		rec := httptest.NewRecorder()
		h.ListDecisions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/decisions", nil), params)
		var out schema.DecisionList
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := call(schema.ListDecisionsParams{})
	if len(out.Data) != 3 || out.Data[0].ChatId != "c1" || out.Data[1].ChatId != "c3" || out.Data[2].ChatId != "c2" {
		t.Errorf("order = %+v, want c1,c3,c2 oldest first", out.Data)
	}
	if out.Skipped == nil || *out.Skipped != 1 || out.Truncated != nil {
		t.Errorf("skipped/truncated = %v/%v, want 1/nil", out.Skipped, out.Truncated)
	}
	two := 2
	out = call(schema.ListDecisionsParams{Limit: &two})
	if len(out.Data) != 2 || out.Data[0].ChatId != "c3" || out.Truncated == nil || !*out.Truncated {
		t.Errorf("limit 2 = %+v truncated %v, want newest two, truncated", out.Data, out.Truncated)
	}
}
