package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/store"
)

// setChatOrigin stamps Origin the way the github extension does; dataset.go branches on State, never Badge.
func setChatOrigin(t *testing.T, st *store.Store, chatID, repo, url, badge string) {
	t.Helper()
	state := extsdk.SubjectOpen
	if badge == "merged" {
		state = extsdk.SubjectMerged
	}
	origin := extsdk.ChatOrigin{
		Extension: "github", Label: repo, Kind: "pr", Href: url, Badge: badge, State: state,
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

// setChatOriginNoState stamps an Origin with no State (badge/Href only), the
// shape a pre-SubjectState extension build wrote before State existed.
func setChatOriginNoState(t *testing.T, st *store.Store, chatID, repo, url, badge string) {
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

func newTestClient(t *testing.T, srv *httptest.Server) *langfuse.Client {
	t.Helper()
	return langfuse.New(srv.URL, "pk", "sk", langfuse.WithHTTPClient(srv.Client()))
}

// llmCallEntry builds a chat entry for a node stream: task -> answer.
func llmCallEntry(chatID, node, agent, task, answer string) ledger.Entry {
	return llmCallRoundEntry(chatID, node, agent, "worker-r0", time.Time{}, task, answer)
}

// llmCallRoundEntry is llmCallEntry with an explicit round and timestamp, for
// tests that need several rounds of the same node (draft/continuation/revise).
func llmCallRoundEntry(chatID, node, agent, round string, at time.Time, task, answer string) ledger.Entry {
	in, _ := json.Marshal([]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: task}}}})
	out, _ := json.Marshal(genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: answer}}})
	payload, _ := json.Marshal(ledger.LLMCallPayload{RequestModel: "m", Input: string(in), Output: string(out)})
	return ledger.Entry{ChatID: chatID, NodeID: node, Agent: agent, Round: round, At: at, Kind: ledger.KindLLMCall, Payload: payload}
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
	lf := newTestClient(t, srv)

	opts := ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"}
	first, _, err := RunDatasetExport(ctx, ls2, st, lf, opts)
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("want 1 item, got %d", len(first))
	}
	second, _, err := RunDatasetExport(ctx, ls2, st, lf, opts)
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

// Item metadata carries prompt_artifact/source/version_id from the llm.call, plus repo and agent.
func TestRunDatasetExport_MetadataBlock(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	entry := llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "looks good")
	var payload ledger.LLMCallPayload
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.PromptSource, payload.PromptVersionID, payload.QuackVersion = "langfuse", "7", "v1.2.3"
	payload.Artifacts = []ledger.ArtifactRef{
		{Name: "system/code-reviewer", Source: "langfuse", VersionID: "7"},
		{Name: "rubric/global", Source: "static", VersionID: "r1"},
	}
	payload.Plugins = []ledger.PluginRef{{Name: "dotagents", SHA: "abc123"}}
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
	lf := newTestClient(t, srv)

	items, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
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
	// The full resolved-artifact/plugin lists ride alongside the
	// single-artifact prompt_* fields, round-tripped through Langfuse's JSON metadata.
	gotArtifacts, _ := json.Marshal(meta["artifacts"])
	wantArtifacts := `[{"name":"system/code-reviewer","source":"langfuse","version_id":"7"},{"name":"rubric/global","source":"static","version_id":"r1"}]`
	if string(gotArtifacts) != wantArtifacts {
		t.Errorf("metadata[artifacts] = %s, want %s", gotArtifacts, wantArtifacts)
	}
	gotPlugins, _ := json.Marshal(meta["plugins"])
	if wantPlugins := `[{"name":"dotagents","sha":"abc123"}]`; string(gotPlugins) != wantPlugins {
		t.Errorf("metadata[plugins] = %s, want %s", gotPlugins, wantPlugins)
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls, st, lf, ExportOpts{Repo: "acme/widget", Dataset: "my-dataset"})
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset", Limit: 1})
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
	lf := newTestClient(t, srv)

	if _, _, err := RunDatasetExport(ctx, ledgertest.NewMemStore(), st, lf, ExportOpts{ChatID: "does-not-exist", Dataset: "my-dataset"}); err == nil {
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ledgertest.NewMemStore(), st, lf,
		ExportOpts{Repo: "acme/widget", Since: time.Now().Add(24 * time.Hour), Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("--since in the future must filter out every chat, got %+v", got)
	}
}

// TestRunDatasetExport_SinceAppliesToSingleChat pins nit 6: --since must
// apply to --chat too, not just --repo (a chat older than --since exports 0).
func TestRunDatasetExport_SinceAppliesToSingleChat(t *testing.T) {
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
	lf := newTestClient(t, srv)

	got, excludedBySince, err := RunDatasetExport(ctx, ledgertest.NewMemStore(), st, lf,
		ExportOpts{ChatID: chat.ID, Since: time.Now().Add(24 * time.Hour), Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("--since in the future must exclude an older --chat, got %+v", got)
	}
	if !excludedBySince {
		t.Fatal("excludedBySince = false, want true so the CLI can report the exclusion instead of a bare zero count")
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
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
	lf := newTestClient(t, srv)

	if _, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"}); err == nil {
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
	lf := newTestClient(t, srv)

	if _, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"}); err == nil {
		t.Fatal("want an error when the dataset create call fails")
	}
}

// The same (chat, node) in two datasets gets two ids: Langfuse item ids are project-scoped.
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
	lf := newTestClient(t, srv)

	a, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "dataset-a"})
	if err != nil {
		t.Fatalf("export dataset-a: %v", err)
	}
	b, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "dataset-b"})
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

// Draft and revise rounds export as one item: task from the draft, expectedOutput from the revise.
func TestRunDatasetExport_DraftPlusRevise(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := ls.AppendIntent(ctx, llmCallRoundEntry("chat-1", "node-1", "code-reviewer", "worker-r0", base, "review this diff", "draft review")); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AppendIntent(ctx, llmCallRoundEntry("chat-1", "node-1", "code-reviewer", "worker-r1", base.Add(time.Minute), "judge feedback + prior answer inlined", "revised review")); err != nil {
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
	setChatOrigin(t, st, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "merged")
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("draft+revise of one node must export exactly one item, got %d: %+v", len(got), got)
	}
	body := items[got[0].ItemID]
	input, _ := body["input"].(map[string]any)
	if input["task"] != "review this diff" {
		t.Fatalf("input.task = %v, want the draft round's task", input["task"])
	}
	if body["expectedOutput"] != "revised review" {
		t.Fatalf("expectedOutput = %v, want the revise round's (delivered) answer", body["expectedOutput"])
	}
}

// A continuation round's answer is the expectedOutput, not the incomplete draft's.
func TestRunDatasetExport_DraftPlusContinuation(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := ls.AppendIntent(ctx, llmCallRoundEntry("chat-1", "node-1", "code-reviewer", "worker-r0", base, "review this diff", "partial draft")); err != nil {
		t.Fatal(err)
	}
	if _, err := ls.AppendIntent(ctx, llmCallRoundEntry("chat-1", "node-1", "code-reviewer", "worker-cont1", base.Add(time.Minute), "continue", "completed review")); err != nil {
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
	setChatOrigin(t, st, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "merged")
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("draft+continuation of one node must export exactly one item, got %d", len(got))
	}
	body := items[got[0].ItemID]
	if body["expectedOutput"] != "completed review" {
		t.Fatalf("expectedOutput = %v, want the continuation's (delivered) answer", body["expectedOutput"])
	}
}

// TestRunDatasetExport_OpenOriginHasNoExpectedOutput pins the finding that an
// unmerged/open chat must never carry an expectedOutput.
func TestRunDatasetExport_OpenOriginHasNoExpectedOutput(t *testing.T) {
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
	setChatOrigin(t, st, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "open")
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 1 || !got[0].NoExpected {
		t.Fatalf("open-origin chat's item must be flagged NoExpected, got %+v", got)
	}
	body := items[got[0].ItemID]
	if _, ok := body["expectedOutput"]; ok && body["expectedOutput"] != nil {
		t.Fatalf("expectedOutput = %v, want absent/nil for an open chat", body["expectedOutput"])
	}
	summary := FormatExportSummary(got)
	if !strings.Contains(summary, "1 without expected output") {
		t.Fatalf("summary %q must report the item exported without expected output", summary)
	}
}

// Origin with empty State (older extension builds) must fall back to the GithubState column.
func TestRunDatasetExport_PreStateOriginFallsBackToGithubState(t *testing.T) {
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
	if err := st.SetChatGitHub(ctx, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "merged", "u"); err != nil {
		t.Fatalf("SetChatGitHub: %v", err)
	}
	setChatOriginNoState(t, st, chat.ID, "acme/widget", "https://github.com/acme/widget/pull/1", "merged")
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
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls2, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 1 || got[0].NoExpected {
		t.Fatalf("pre-State merged chat's item must have expectedOutput (fall through to GithubState), got %+v", got)
	}
}

// --agent keeps a mixed chat's dataset to the one agent an experiment re-runs; an agent export never covers is an error.
func TestRunDatasetExport_AgentFilter(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	ls := ledgertest.NewMemStore()
	for _, e := range []ledger.Entry{
		llmCallEntry(chat.ID, "node-1", "code-reviewer", "review this", "looks good"),
		llmCallEntry(chat.ID, "node-2", "synthesizer", "combine these", "combined"),
	} {
		if _, err := ls.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	fake := newFakeLangfuse(t)
	srv := fake.server()
	defer srv.Close()
	lf := newTestClient(t, srv)

	got, _, err := RunDatasetExport(ctx, ls, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset", Agent: "synthesizer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Agent != "synthesizer" || got[0].NodeID != "node-2" {
		t.Fatalf("items = %+v, want only the synthesizer's node-2", got)
	}
	if _, _, err := RunDatasetExport(ctx, ls, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset", Agent: "advisor"}); err == nil {
		t.Fatal("want an error for an agent dataset export doesn't cover")
	}

	// The two agents this PR adds must actually export, not just be accepted by --agent.
	for _, e := range []ledger.Entry{
		llmCallEntry(chat.ID, "node-3", "web-researcher", "research this", "researched"),
		llmCallEntry(chat.ID, "node-4", "trend-scout", "scan trends", "scanned"),
	} {
		if _, err := ls.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for agent, node := range map[string]string{"web-researcher": "node-3", "trend-scout": "node-4"} {
		got, _, err = RunDatasetExport(ctx, ls, st, lf, ExportOpts{ChatID: chat.ID, Dataset: "my-dataset", Agent: agent})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Agent != agent || got[0].NodeID != node {
			t.Fatalf("items = %+v, want only the %s's %s", got, agent, node)
		}
	}
}

// A sleeper job reruns one node id under the same round ids every turn: each turn is its own item.
func TestRunDatasetExport_RecurringNodeExportsOneItemPerTurn(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chatID := "ext:sleeper:1:6:trends"
	setChatOrigin(t, st, chatID, "", "", "")
	ls := ledgertest.NewMemStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, turn := range []string{"t1", "t2", "t3"} {
		at := base.Add(time.Duration(i) * time.Hour)
		started := ledger.Entry{ChatID: chatID, TurnID: turn, NodeID: "trends", Kind: ledger.KindNodeStarted, At: at}
		call := llmCallRoundEntry(chatID, "trends", "trend-scout", "worker-r0", at.Add(time.Second), "scout "+turn, "trends "+turn)
		for _, e := range []ledger.Entry{started, call} {
			if _, err := ls.AppendIntent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	fake := newFakeLangfuse(t)
	srv := fake.server()
	defer srv.Close()
	lf := newTestClient(t, srv)

	opts := ExportOpts{ChatID: chatID, Dataset: "my-dataset"}
	first, _, err := RunDatasetExport(ctx, ls, st, lf, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := RunDatasetExport(ctx, ls, st, lf, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || len(fake.items) != 3 {
		t.Fatalf("want 3 items (one per turn) upserted twice, got %d exported, %d distinct", len(first), len(fake.items))
	}
	for i := range first {
		if second[i].ItemID != first[i].ItemID {
			t.Fatalf("re-export item %d id %s, want %s", i, second[i].ItemID, first[i].ItemID)
		}
	}
	opts.Limit = 1
	newest, _, err := RunDatasetExport(ctx, ls, st, lf, opts)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := fake.items[newest[0].ItemID]["metadata"].(map[string]any)
	if meta["turn_id"] != "t3" {
		t.Fatalf("--limit 1 exported turn %v, want the newest, t3", meta["turn_id"])
	}
	if input, _ := fake.items[newest[0].ItemID]["input"].(map[string]any); input["setup"] != nil {
		t.Fatalf("a trend-scout item carries setup %v; it replays with no clone", input["setup"])
	}
}

// Each turn of a PR review exports the dispatched clone pinned to the head that turn's node started on, read
// from its revisions' lineage, so a replay reviews the code the original run saw.
func TestRunDatasetExport_ReviewerItemCarriesPinnedClone(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chatID := "ext:github:github-acme-widget-1"
	setup := extsdk.Setup{Repo: "https://github.com/acme/widget.git", BaseRef: "main", WorkBranch: "quack/issue-1", ExistingHeadRef: "feat/x"}
	origin, _ := json.Marshal(struct {
		extsdk.ChatOrigin
		Setup extsdk.Setup `json:"quackSetup"`
	}{extsdk.ChatOrigin{Extension: "github", Href: "https://github.com/acme/widget/pull/1"}, setup})
	if err := st.SetChatOrigin(ctx, chatID, "u", string(origin)); err != nil {
		t.Fatal(err)
	}
	ls := ledgertest.NewMemStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	heads := map[string]string{"t1": strings.Repeat("1", 40), "t2": strings.Repeat("2", 40)}
	for i, turn := range []string{"t1", "t2"} {
		at := base.Add(time.Duration(i) * time.Hour)
		rev := fmt.Sprintf(`{"id":"review_record:%s","revision":1,"lineage":{"node_id":"review","head_sha":%q}}`, turn, heads[turn])
		for _, e := range []ledger.Entry{
			{ChatID: chatID, TurnID: turn, NodeID: "review", Kind: ledger.KindNodeStarted, At: at},
			{ChatID: chatID, TurnID: turn, NodeID: "review", Kind: ledger.KindArtifactRevision, Key: "review_record:" + turn, At: at.Add(time.Second), Payload: json.RawMessage(rev)},
			llmCallRoundEntry(chatID, "review", "code-reviewer", "worker-r0", at.Add(2*time.Second), "review "+turn, "verdict "+turn),
		} {
			if _, err := ls.AppendIntent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	fake := newFakeLangfuse(t)
	srv := fake.server()
	defer srv.Close()

	items, _, err := RunDatasetExport(ctx, ls, st, newTestClient(t, srv), ExportOpts{ChatID: chatID, Dataset: "my-dataset"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want one item per review turn, got %+v", items)
	}
	if s := FormatExportSummary(items); !strings.Contains(s, "2 with a replay clone") {
		t.Fatalf("summary %q must count the items carrying a clone", s)
	}
	for _, it := range items {
		body := fake.items[it.ItemID]
		turn := body["metadata"].(map[string]any)["turn_id"].(string)
		b, _ := json.Marshal(body["input"])
		var in experimentItemInput
		if err := json.Unmarshal(b, &in); err != nil {
			t.Fatal(err)
		}
		want := setup
		want.HeadSHA = heads[turn]
		if in.Task != "review "+turn || in.Setup == nil || *in.Setup != want {
			t.Fatalf("turn %s input = %s, want task %q in %+v", turn, b, "review "+turn, want)
		}
	}
}

// TestRunDatasetExport_ItemCarriesManifestArtifacts: a replay runs in a fresh chat, so the item carries each input
// artifact its task's <artifacts> manifest lists, at the listed revision; one gone from the chat is left out.
func TestRunDatasetExport_ItemCarriesManifestArtifacts(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chatID := "ext:github:github-acme-widget-1"
	if err := st.SetChatOrigin(ctx, chatID, "u", ""); err != nil {
		t.Fatal(err)
	}
	chat, err := st.GetChat(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, artifactref.AppName, store.SessionUserFor(*chat), chatID)
	for _, body := range []string{`{"title":"as listed"}`, `{"title":"edited later"}`} {
		if _, _, err := rc.SaveBlob(ctx, "bytes", []byte(body), "application/json", "issue", recordstore.Lineage{}); err != nil {
			t.Fatal(err)
		}
	}
	task := "review\n<artifacts>\n  <artifact id=\"bytes:issue\" revision=\"1\" status=\"new\">1 object</artifact>\n" +
		"  <artifact id=\"bytes:gone\" revision=\"1\" status=\"new\">1 object</artifact>\n</artifacts>\n"
	ls := ledgertest.NewMemStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range []ledger.Entry{
		{ChatID: chatID, TurnID: "t1", NodeID: "review", Kind: ledger.KindNodeStarted, At: at},
		llmCallRoundEntry(chatID, "review", "code-reviewer", "worker-r0", at.Add(time.Second), task, "verdict"),
	} {
		if _, err := ls.AppendIntent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	fake := newFakeLangfuse(t)
	srv := fake.server()
	defer srv.Close()

	items, _, err := RunDatasetExport(ctx, ls, st, newTestClient(t, srv), ExportOpts{ChatID: chatID, Dataset: "my-dataset", Artifacts: svc})
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v, err = %v; want one", items, err)
	}
	b, _ := json.Marshal(fake.items[items[0].ItemID]["input"])
	var in experimentItemInput
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Artifacts) != 1 || !strings.Contains(in.Artifacts["issue"], "as listed") {
		t.Fatalf("item artifacts = %v, want only issue at revision 1", in.Artifacts)
	}
}

// A resumed (or paused-then-continued) run opens a second node.started under a new turn; it folds into the
// run it carries on, so the item keeps the original task rather than the continue prompt.
func TestRunDatasetExport_ResumedRunFoldsIntoItsTurn(t *testing.T) {
	for _, resumed := range []string{`{"resumed_from":"ctx-1"}`, `{"continued":true}`} {
		ctx := context.Background()
		st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
		if err != nil {
			t.Fatal(err)
		}
		chatID := "ext:sleeper:1:6:trends"
		setChatOrigin(t, st, chatID, "", "", "")
		ls := ledgertest.NewMemStore()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, e := range []ledger.Entry{
			{ChatID: chatID, TurnID: "t1", NodeID: "trends", Kind: ledger.KindNodeStarted, At: base},
			llmCallRoundEntry(chatID, "trends", "trend-scout", "worker-r0", base.Add(time.Second), "scout the league", "partial"),
			{ChatID: chatID, TurnID: "t2", NodeID: "trends", Kind: ledger.KindNodeStarted, At: base.Add(time.Hour), Payload: json.RawMessage(resumed)},
			llmCallRoundEntry(chatID, "trends", "trend-scout", "worker-continue", base.Add(time.Hour+time.Second), "continue", "final trends"),
		} {
			if _, err := ls.AppendIntent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		fake := newFakeLangfuse(t)
		srv := fake.server()
		got, _, err := RunDatasetExport(ctx, ls, st, newTestClient(t, srv), ExportOpts{ChatID: chatID, Dataset: "my-dataset"})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: want the resumed run folded into one item, got %d", resumed, len(got))
		}
		input, _ := fake.items[got[0].ItemID]["input"].(map[string]any)
		if input["task"] != "scout the league" {
			t.Fatalf("%s: task = %v, want the original task", resumed, input["task"])
		}
	}
}

// `quack experiment run` replays items in ext:experiment: chats; --since must not export them back.
func TestRunDatasetExport_SinceSkipsExperimentChats(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	replay := "ext:experiment:4f1c"
	setChatOrigin(t, st, replay, "", "", "")
	ls := ledgertest.NewMemStore()
	for _, id := range []string{live.ID, replay} {
		if _, err := ls.AppendIntent(ctx, llmCallEntry(id, "node-1", "synthesizer", "summarize", "summary")); err != nil {
			t.Fatal(err)
		}
	}
	var items map[string]map[string]any
	srv := datasetExistsServer(t, &items)
	defer srv.Close()

	got, _, err := RunDatasetExport(ctx, ls, st, newTestClient(t, srv), ExportOpts{Since: time.Now().Add(-time.Hour), Dataset: "my-dataset"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChatID != live.ID {
		t.Fatalf("want only the real chat's item, got %+v", got)
	}
}
