package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/store"
)

// setChatOrigin stamps chatID's Origin the way the github extension's
// chatOrigin/refreshChatOrigin do, so tests exercise the real production write path.
func setChatOrigin(t *testing.T, st *store.Store, chatID, repo, url, badge string) {
	t.Helper()
	origin := extsdk.ChatOrigin{
		Extension: "github", Label: repo, Kind: "pr", Href: url, Badge: badge,
		Labels: map[string][]extsdk.LabelValue{"repo": {{Value: repo}}},
	}
	b, err := json.Marshal(origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChatOrigin(context.Background(), chatID, "u", string(b)); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
}

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

// TestRunDatasetExport_MetadataBlock pins issue #1424 item 16: the exported item's
// metadata carries prompt_artifact/prompt_source/prompt_version_id (from the recorded
// llm.call), plus repo (from Origin) and agent.
func TestRunDatasetExport_MetadataBlock(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	entry := llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "looks good")
	var payload ledger.LLMCallPayload
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.PromptSource, payload.PromptVersionID, payload.QuackVersion = "langfuse", "7", "v1.2.3"
	entry.Payload, _ = json.Marshal(payload)
	if _, err := ls.AppendIntent(ctx, entry); err != nil {
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
	setChatOrigin(t, st, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "open")
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

	items, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	meta, _ := fake.items[items[0].ItemID]["metadata"].(map[string]any)
	want := map[string]any{
		"repo": "acme/widget", "agent": "code-reviewer",
		"prompt_artifact": "system/code-reviewer", "prompt_source": "langfuse", "prompt_version_id": "7",
	}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("metadata[%q] = %v, want %v (full metadata: %+v)", k, meta[k], v, meta)
		}
	}
}

// datasetExistsServer answers DatasetsGet with 200 (already exists) instead of 404, so
// ensureDataset's "no create needed" branch runs.
func datasetExistsServer(t *testing.T, items *map[string]map[string]any) *httptest.Server {
	*items = map[string]map[string]any{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/v2/datasets/my-dataset" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"id": "ds1", "name": "my-dataset", "projectId": "p1",
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
		case r.URL.Path == "/api/public/dataset-items" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			(*items)[id] = body
			writeJSON(w, map[string]any{"id": id, "datasetId": "ds1", "datasetName": "my-dataset", "createdAt": "2026-01-01T00:00:00Z"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestRunDatasetExport_ByRepoAndSince(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	inRepo, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	setChatOrigin(t, st, inRepo.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "merged")
	otherRepo, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	setChatOrigin(t, st, otherRepo.ID, "acme/other", "https://github.com/acme/other/pull/2", "merged")

	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry(inRepo.ID, "node-1", "synthesizer", "summarize this", "the summary")); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AppendIntent(ctx, llmCallEntry(otherRepo.ID, "node-1", "synthesizer", "summarize that", "ignored")); err != nil {
		t.Fatal(err)
	}

	var items map[string]map[string]any
	srv := datasetExistsServer(t, &items)
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	got, err := RunDatasetExport(ctx, ls, st, lf, ExportOpts{Repo: "acme/widget", Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 1 || got[0].ChatID != inRepo.ID {
		t.Fatalf("want exactly the acme/widget chat's item, got %+v", got)
	}
	body := items[got[0].ItemID]
	if body["expectedOutput"] != "the summary" {
		t.Fatalf("expectedOutput = %v, want the merged chat's answer", body["expectedOutput"])
	}
}

func TestRunDatasetExport_LimitStopsEarly(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-1", "code-reviewer", "review A", "ok A")); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-2", "synthesizer", "review B", "ok B")); err != nil {
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
	entries, _ := ls.ReadEntries(ctx, "chat-1", 0)
	ls2 := ledgertest.NewMemStore()
	for _, e := range entries {
		e.ChatID = chat.ID
		if _, err := ls2.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	var items map[string]map[string]any
	srv := datasetExistsServer(t, &items)
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	got, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset", Limit: 1})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("--limit 1 must stop after one item, got %d", len(got))
	}
}

func TestRunDatasetExport_UnknownChatID(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	var items map[string]map[string]any
	srv := datasetExistsServer(t, &items)
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	if _, err := RunDatasetExport(ctx, ledgertest.NewMemStore(), st, lf, ExportOpts{ChatID: "does-not-exist", Dataset: "my-dataset"}); err == nil {
		t.Fatal("want an error, not a panic, for an unknown --chat id")
	}
}

func TestRunDatasetExport_SinceFiltersOutOlderChats(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	setChatOrigin(t, st, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "merged")

	var items map[string]map[string]any
	srv := datasetExistsServer(t, &items)
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	got, err := RunDatasetExport(ctx, ledgertest.NewMemStore(), st, lf,
		ExportOpts{Repo: "acme/widget", Since: time.Now().Add(24 * time.Hour), Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("--since in the future must filter out every chat, got %+v", got)
	}
}

func TestRunDatasetExport_SkipsChatsWithNoRecording(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	ls := ledgertest.NewMemStore() // no entries for chat.ID at all -> ErrNoRecording

	var items map[string]map[string]any
	srv := datasetExistsServer(t, &items)
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	got, err := RunDatasetExport(ctx, ls, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export must skip a chat with no recording, not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want zero items, got %+v", got)
	}
}

func TestRunDatasetExport_ItemCreateFailurePropagates(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "ok")); err != nil {
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
	entries, _ := ls.ReadEntries(ctx, "chat-1", 0)
	ls2 := ledgertest.NewMemStore()
	for _, e := range entries {
		e.ChatID = chat.ID
		if _, err := ls2.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/v2/datasets/my-dataset":
			writeJSON(w, map[string]any{"id": "ds1", "name": "my-dataset", "projectId": "p1",
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
		case r.URL.Path == "/api/public/dataset-items":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	if _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"}); err == nil {
		t.Fatal("want an error when the dataset-item create call fails")
	}
}

// TestRunDatasetExport_DatasetCreateFailurePropagates also pins that ensureDataset
// runs lazily: it must not be called (and so must not fail) until a real item is ready.
func TestRunDatasetExport_DatasetCreateFailurePropagates(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "ok")); err != nil {
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
	entries, _ := ls.ReadEntries(ctx, "chat-1", 0)
	ls2 := ledgertest.NewMemStore()
	for _, e := range entries {
		e.ChatID = chat.ID
		if _, err := ls2.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/v2/datasets/my-dataset":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/public/v2/datasets":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	if _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"}); err == nil {
		t.Fatal("want an error when the dataset create call fails")
	}
}

// TestRunDatasetExport_ItemIDsAreDatasetScoped pins issue #1424 item 7: Langfuse
// dataset item ids are project-scoped and cannot be reused across datasets, so the
// same (chat, node) exported to two different datasets must get two different ids.
func TestRunDatasetExport_ItemIDsAreDatasetScoped(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "ok")); err != nil {
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
	entries, _ := ls.ReadEntries(ctx, "chat-1", 0)
	ls2 := ledgertest.NewMemStore()
	for _, e := range entries {
		e.ChatID = chat.ID
		if _, err := ls2.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	items := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/v2/datasets/dataset-a" || r.URL.Path == "/api/public/v2/datasets/dataset-b":
			writeJSON(w, map[string]any{"id": "ds1", "name": "d", "projectId": "p1",
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
		case r.URL.Path == "/api/public/dataset-items" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			items[id] = body
			writeJSON(w, map[string]any{"id": id, "datasetId": "ds1", "datasetName": "d", "createdAt": "2026-01-01T00:00:00Z"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	a, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "dataset-a"})
	if err != nil {
		t.Fatalf("export dataset-a: %v", err)
	}
	b, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "dataset-b"})
	if err != nil {
		t.Fatalf("export dataset-b: %v", err)
	}
	if len(a) != 1 || len(b) != 1 || a[0].ItemID == b[0].ItemID {
		t.Fatalf("want distinct item ids across datasets, got %+v and %+v", a, b)
	}
	if len(items) != 2 {
		t.Fatalf("want two distinct stored items, got %d", len(items))
	}
}
