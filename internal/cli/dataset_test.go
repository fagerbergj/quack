package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/store"
)

// fakeLangfuse records every request body it receives, keyed by method+path, and answers
// dataset-item creates as an upsert (so a second export of the same id overwrites, not appends).
type fakeLangfuse struct {
	t        *testing.T
	requests []string
	items    map[string]map[string]any // id -> decoded body
}

func newFakeLangfuse(t *testing.T) *fakeLangfuse {
	return &fakeLangfuse{t: t, items: map[string]map[string]any{}}
}

func (f *fakeLangfuse) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/api/public/v2/datasets/my-dataset" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/public/v2/datasets" && r.Method == http.MethodPost:
			writeJSON(w, map[string]any{"id": "ds1", "name": "my-dataset", "projectId": "p1",
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
		case r.URL.Path == "/api/public/dataset-items" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			f.items[id] = body
			writeJSON(w, map[string]any{"id": id, "datasetId": "ds1", "datasetName": "my-dataset",
				"createdAt": "2026-01-01T00:00:00Z"})
		default:
			f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestGenClient(t *testing.T, srv *httptest.Server) *langfusegen.ClientWithResponses {
	c, err := langfuse.NewGenClient(srv.URL, "pk", "sk", langfuse.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewGenClient: %v", err)
	}
	return c
}

// llmCallEntry builds a chat entry for a node stream: task -> answer.
func llmCallEntry(chatID, node, agent, task, answer string) ledger.Entry {
	in, _ := json.Marshal([]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: task}}}})
	out, _ := json.Marshal(genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: answer}}})
	payload, _ := json.Marshal(ledger.LLMCallPayload{RequestModel: "m", Input: string(in), Output: string(out)})
	return ledger.Entry{ChatID: chatID, NodeID: node, Agent: agent, Kind: ledger.KindLLMCall, Payload: payload}
}

func TestRunDatasetExport_Idempotent(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "looks good")); err != nil {
		t.Fatal(err)
	}
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// Re-point the ledger entries at the real chat id CreateChat assigned.
	entries, _ := ls.ReadEntries(ctx, "chat-1", 0)
	ls2 := ledgertest.NewMemStore()
	for _, e := range entries {
		e.ChatID = chat.ID
		if _, err := ls2.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	fake := newFakeLangfuse(t)
	srv := fake.server()
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	opts := ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"}
	first, err := RunDatasetExport(ctx, ls2, st, lf, opts)
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("want 1 item, got %d", len(first))
	}
	second, err := RunDatasetExport(ctx, ls2, st, lf, opts)
	if err != nil {
		t.Fatalf("second export: %v", err)
	}
	if len(second) != 1 || second[0].ItemID != first[0].ItemID {
		t.Fatalf("re-export must reuse the same item id: first=%+v second=%+v", first, second)
	}
	if len(fake.items) != 1 {
		t.Fatalf("want exactly one distinct item after two exports, got %d", len(fake.items))
	}
	body := fake.items[first[0].ItemID]
	input, _ := body["input"].(map[string]any)
	if input["task"] != "review this" {
		t.Fatalf("input.task = %v, want %q", input["task"], "review this")
	}
}
