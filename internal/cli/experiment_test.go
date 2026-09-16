package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/store"
)

func datasetItemFromInput(t *testing.T, id string, input map[string]any) langfusegen.DatasetItem {
	t.Helper()
	return langfusegen.DatasetItem{Id: id, Input: input}
}

// stubRunner is a fake itemRunner: returns a fixed (answer, traceID) per task, or errs
// when task matches errOn - lets RunExperiment's loop be tested without a live server.
type stubRunner struct {
	traceID string
	errOn   string
}

func (r *stubRunner) RunItem(_ context.Context, task string) (string, string, error) {
	if task == r.errOn {
		return "", "", fmt.Errorf("boom")
	}
	return "answer for " + task, r.traceID, nil
}

func TestRunExperiment_ReportsRealTraceID(t *testing.T) {
	var runItemBodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/dataset-items" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"data": []map[string]any{
				{"id": "item1", "input": map[string]any{"task": "review this"}},
			}, "meta": map[string]any{"page": 1, "limit": 1, "totalItems": 1, "totalPages": 1}})
		case r.URL.Path == "/api/public/dataset-run-items" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			runItemBodies = append(runItemBodies, body)
			writeJSON(w, map[string]any{"id": "ri1", "datasetRunId": "run1", "datasetItemId": body["datasetItemId"],
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	runner := &stubRunner{traceID: "trace-abc123"}
	results, err := RunExperiment(context.Background(), io.Discard, runner, lf,
		ExperimentOpts{Dataset: "my-dataset", Agent: "code-reviewer", Prompt: "system/code-reviewer@3", RunName: "run1"})
	if err != nil {
		t.Fatalf("RunExperiment: %v", err)
	}
	if len(results) != 1 || results[0].TraceID != "trace-abc123" {
		t.Fatalf("want one result with the runner's trace id, got %+v", results)
	}
	if len(runItemBodies) != 1 || runItemBodies[0]["traceId"] != "trace-abc123" {
		t.Fatalf("run item traceId must equal the node's emitted trace id, got %+v", runItemBodies)
	}
}

func TestRunExperiment_ItemErrorIsReportedNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/public/dataset-items" {
			writeJSON(w, map[string]any{"data": []map[string]any{
				{"id": "item1", "input": map[string]any{"task": "fail me"}},
			}, "meta": map[string]any{"page": 1, "limit": 1, "totalItems": 1, "totalPages": 1}})
			return
		}
		t.Fatalf("run item should not be created for a failed run: %s", r.URL.Path)
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	runner := &stubRunner{errOn: "fail me"}
	results, err := RunExperiment(context.Background(), io.Discard, runner, lf, ExperimentOpts{Dataset: "my-dataset", RunName: "run1"})
	if err != nil {
		t.Fatalf("RunExperiment: %v", err)
	}
	if len(results) != 1 || results[0].Error == "" {
		t.Fatalf("want one errored result, got %+v", results)
	}
}

func TestRunExperiment_ListDatasetItemsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	_, err := RunExperiment(context.Background(), io.Discard, &stubRunner{}, lf, ExperimentOpts{Dataset: "my-dataset", Limit: 5})
	if err == nil {
		t.Fatal("want an error when the dataset-items list call fails")
	}
}

func TestRunExperiment_MissingTaskStopsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item1", "input": map[string]any{}}},
			"meta": map[string]any{"page": 1, "limit": 1, "totalItems": 1, "totalPages": 1}})
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	_, err := RunExperiment(context.Background(), io.Discard, &stubRunner{}, lf, ExperimentOpts{Dataset: "my-dataset"})
	if err == nil {
		t.Fatal("want an error when a dataset item has no task field")
	}
}

func TestRunExperiment_RecordRunItemError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/dataset-items":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item1", "input": map[string]any{"task": "x"}}},
				"meta": map[string]any{"page": 1, "limit": 1, "totalItems": 1, "totalPages": 1}})
		case r.URL.Path == "/api/public/dataset-run-items":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	_, err := RunExperiment(context.Background(), io.Discard, &stubRunner{}, lf, ExperimentOpts{Dataset: "my-dataset"})
	if err == nil {
		t.Fatal("want an error when creating the run item fails")
	}
}

func TestLiveItemRunner_TraceIDFor(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := st.CreateChat(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDagPlan(ctx, chat.ID, "plan1", "turn1", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDagNode(ctx, store.DagNode{NodeID: "node-1", PlanID: "plan1", TraceID: "trace-real-1", TraceIDSet: true}); err != nil {
		t.Fatal(err)
	}

	r := &LiveItemRunner{Store: st, Agent: "code-reviewer"}
	got := r.traceIDFor(ctx, chat.ID, "node-1")
	if got != "trace-real-1" {
		t.Fatalf("traceIDFor = %q, want the node's recorded trace id", got)
	}
	if got := r.traceIDFor(ctx, chat.ID, "no-such-node"); got != "" {
		t.Fatalf("traceIDFor for an unknown node = %q, want empty", got)
	}
	if got := (&LiveItemRunner{}).traceIDFor(ctx, chat.ID, "node-1"); got != "" {
		t.Fatalf("traceIDFor with no Store = %q, want empty", got)
	}
}

func TestSessionFromBundleBytes_RoundTrips(t *testing.T) {
	ctx := context.Background()
	ls := ledgertest.NewMemStore()
	if _, err := ls.AppendIntent(ctx, llmCallEntry("chat-1", "node-1", "code-reviewer", "review this", "ok")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ledger.AssembleBundle(ctx, ls, "chat-1", "test-version", &buf); err != nil {
		t.Fatal(err)
	}

	sess, err := sessionFromBundleBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("sessionFromBundleBytes: %v", err)
	}
	runs := sess.NodeRuns(map[string]bool{"code-reviewer": true})
	if len(runs) != 1 {
		t.Fatalf("want one node run out of the round-tripped bundle, got %+v", runs)
	}
}

func TestFormatExperimentSummary(t *testing.T) {
	out := FormatExperimentSummary([]ExperimentResult{
		{ItemID: "item1", TraceID: "trace1", Prompt: "system/code-reviewer@3", Duration: 2500 * time.Millisecond},
		{ItemID: "item2", Error: "boom"},
	})
	if !bytes.Contains([]byte(out), []byte("item1")) || !bytes.Contains([]byte(out), []byte("trace1")) {
		t.Fatalf("summary missing item1/trace1: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("boom")) {
		t.Fatalf("summary missing the errored item's message: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("2 item(s) run")) {
		t.Fatalf("summary missing the item count: %s", out)
	}
}

func TestItemTask(t *testing.T) {
	item := datasetItemFromInput(t, "item1", map[string]any{"task": "do the thing"})
	task, err := itemTask(item)
	if err != nil {
		t.Fatal(err)
	}
	if task != "do the thing" {
		t.Fatalf("task = %q", task)
	}

	if _, err := itemTask(datasetItemFromInput(t, "item1", map[string]any{})); err == nil {
		t.Fatal("want error for missing task field")
	}
}
