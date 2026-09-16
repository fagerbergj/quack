package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/langfuse/langfusegen"
)

func datasetItemFromInput(t *testing.T, input map[string]any) langfusegen.DatasetItem {
	t.Helper()
	return langfusegen.DatasetItem{Id: "item1", Input: input}
}

func TestRecordRunItem_TraceIDMatchesEmitted(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/dataset-run-items" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, map[string]any{"id": "ri1", "datasetRunId": "run1", "datasetItemId": gotBody["datasetItemId"],
			"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	lf := newTestGenClient(t, srv)

	traceID, err := newTraceID()
	if err != nil {
		t.Fatal(err)
	}
	opts := ExperimentOpts{RunName: "run1", Agent: "code-reviewer", Prompt: "system/code-reviewer@3"}
	if err := recordRunItem(context.Background(), lf, opts, "item1", "chat1", traceID, "the answer"); err != nil {
		t.Fatalf("recordRunItem: %v", err)
	}
	if gotBody["traceId"] != traceID {
		t.Fatalf("run item traceId = %v, want the emitted trace id %q", gotBody["traceId"], traceID)
	}
	if gotBody["datasetItemId"] != "item1" || gotBody["runName"] != "run1" {
		t.Fatalf("unexpected run item body: %+v", gotBody)
	}
}

func TestItemTask(t *testing.T) {
	item := datasetItemFromInput(t, map[string]any{"task": "do the thing"})
	task, err := itemTask(item)
	if err != nil {
		t.Fatal(err)
	}
	if task != "do the thing" {
		t.Fatalf("task = %q", task)
	}

	if _, err := itemTask(datasetItemFromInput(t, map[string]any{})); err == nil {
		t.Fatal("want error for missing task field")
	}
}
