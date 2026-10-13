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
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
)

func datasetItemFromInput(t *testing.T, id string, input map[string]any) langfuse.DatasetItem {
	t.Helper()
	return langfuse.DatasetItem{ID: id, Input: input}
}

// stubRunner is a fake itemRunner: returns run with traceID per task, or errs
// when task matches errOn - lets RunExperiment's loop be tested without a live server.
type stubRunner struct {
	traceID string
	errOn   string
	run     itemRun
}

func (r *stubRunner) RunItem(_ context.Context, task string) (itemRun, error) {
	if task == r.errOn {
		return itemRun{}, fmt.Errorf("boom")
	}
	run := r.run
	run.Answer, run.TraceID = "answer for "+task, r.traceID
	return run, nil
}

// ingestOK answers an ingestion batch with no per-event failures.
func ingestOK(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"successes": []any{}, "errors": []any{}})
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
		case r.URL.Path == "/api/public/ingestion":
			ingestOK(w)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

	runner := &stubRunner{traceID: "trace-abc123", run: itemRun{Prompt: ledger.ArtifactRef{Name: "system/code-reviewer", Source: "langfuse", VersionID: "3"}}}
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
	lf := newTestClient(t, srv)

	runner := &stubRunner{errOn: "fail me"}
	results, err := RunExperiment(context.Background(), io.Discard, runner, lf, ExperimentOpts{Dataset: "my-dataset", RunName: "run1"})
	if err != nil {
		t.Fatalf("RunExperiment: %v", err)
	}
	if len(results) != 1 || results[0].Error == "" {
		t.Fatalf("want one errored result, got %+v", results)
	}
}

// TestListDatasetItems_PagesUntilEmpty drives a 2-page fake: page 1 returns one item
// with totalPages=2, page 2 returns the second and last item.
func TestListDatasetItems_PagesUntilEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item1", "input": map[string]any{"task": "a"}}},
				"meta": map[string]any{"page": 1, "limit": 1, "totalItems": 2, "totalPages": 2}})
		case "2":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item2", "input": map[string]any{"task": "b"}}},
				"meta": map[string]any{"page": 2, "limit": 1, "totalItems": 2, "totalPages": 2}})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

	items, err := listDatasetItems(context.Background(), lf, "my-dataset", 0)
	if err != nil {
		t.Fatalf("listDatasetItems: %v", err)
	}
	if len(items) != 2 || items[0].ID != "item1" || items[1].ID != "item2" {
		t.Fatalf("want both pages' items in order, got %+v", items)
	}
}

// TestListDatasetItems_LimitStopsBeforeSecondPage: --limit 1 must not fetch page 2.
func TestListDatasetItems_LimitStopsBeforeSecondPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			t.Fatalf("must not fetch beyond the limit, got page %q", r.URL.Query().Get("page"))
		}
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item1", "input": map[string]any{"task": "a"}}},
			"meta": map[string]any{"page": 1, "limit": 1, "totalItems": 2, "totalPages": 2}})
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

	items, err := listDatasetItems(context.Background(), lf, "my-dataset", 1)
	if err != nil {
		t.Fatalf("listDatasetItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want exactly 1 item, got %d", len(items))
	}
}

func TestRunExperiment_ListDatasetItemsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

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
	lf := newTestClient(t, srv)

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
		case r.URL.Path == "/api/public/ingestion":
			ingestOK(w)
		case r.URL.Path == "/api/public/dataset-run-items":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

	_, err := RunExperiment(context.Background(), io.Discard, &stubRunner{traceID: "t1"}, lf, ExperimentOpts{Dataset: "my-dataset"})
	if err == nil {
		t.Fatal("want an error when creating the run item fails")
	}
}

// A hard error on item 2 must still print item 1's completed row.
func TestRunExperiment_HardErrorReportsCompletedItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/dataset-items":
			writeJSON(w, map[string]any{"data": []map[string]any{
				{"id": "item1", "input": map[string]any{"task": "a"}},
				{"id": "item2", "input": map[string]any{"task": "b"}},
			}, "meta": map[string]any{"page": 1, "limit": 2, "totalItems": 2, "totalPages": 1}})
		case r.URL.Path == "/api/public/dataset-run-items":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["datasetItemId"] == "item2" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"id": "ri1", "datasetRunId": "run1", "datasetItemId": body["datasetItemId"],
				"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
		case r.URL.Path == "/api/public/ingestion":
			ingestOK(w)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

	var errOut bytes.Buffer
	_, err := RunExperiment(context.Background(), &errOut, &stubRunner{traceID: "t1"}, lf,
		ExperimentOpts{Dataset: "my-dataset", RunName: "run1"})
	if err == nil {
		t.Fatal("want an error when creating item 2's run item fails")
	}
	if !bytes.Contains(errOut.Bytes(), []byte("item1")) {
		t.Fatalf("completed item 1 must reach the partial summary on errOut, got %q", errOut.String())
	}
}

// recordingBody builds a FetchRecording response body: one node run for chatID/node/agent
// with the given answer, assembled the same way the real ledger does.
func recordingBody(t *testing.T, chatID, node, agent, task, answer string) []byte {
	t.Helper()
	return recordingOf(t, chatID, llmCallEntry(chatID, node, agent, task, answer))
}

func recordingOf(t *testing.T, chatID string, entries ...ledger.Entry) []byte {
	t.Helper()
	ls := ledgertest.NewMemStore()
	for _, e := range entries {
		if _, err := ls.AppendIntent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := ledger.AssembleBundle(context.Background(), ls, chatID, "test-version", &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func evalScoreEntry(node, round string, at time.Time, criterion string, score float64) ledger.Entry {
	payload, _ := json.Marshal(ledger.EvalScorePayload{ResponseID: round, Criterion: criterion, Score: score, Explanation: criterion + " why"})
	return ledger.Entry{ChatID: "c1", NodeID: node, Agent: "judge", Round: round, At: at, Kind: ledger.KindEvalScore, Payload: payload}
}

// workerCall is one synthesizer llm.call on n1: history in, out out.
func workerCall(round string, at time.Time, out *genai.Content, arts []ledger.ArtifactRef, in ...*genai.Content) ledger.Entry {
	inJSON, _ := json.Marshal(in)
	outJSON, _ := json.Marshal(out)
	payload, _ := json.Marshal(ledger.LLMCallPayload{RequestModel: "m", Input: string(inJSON), Output: string(outJSON), Artifacts: arts})
	return ledger.Entry{ChatID: "c1", NodeID: "n1", Agent: "synthesizer", Round: round, At: at, Kind: ledger.KindLLMCall, Payload: payload}
}

// synthesizerRecording: a draft round with two tool calls (the first answered with an error), judged
// once, then a revise round judged again - the second judge round is the delivered verdict.
func synthesizerRecording(t *testing.T) []byte {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	user := genai.NewContentFromText("synthesize this", genai.RoleUser)
	call := func(id string) *genai.Content {
		return &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: "read_artifact"}}}}
	}
	resp := func(id string, r map[string]any) *genai.Content {
		return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: id, Name: "read_artifact", Response: r}}}}
	}
	bad, good := resp("c1", map[string]any{"error": "not found"}), resp("c2", map[string]any{"result": "ok"})
	arts := []ledger.ArtifactRef{{Name: "system/synthesizer", Source: "langfuse", VersionID: "4"}}
	return recordingOf(t, "c1",
		workerCall("worker-r0", at(1), call("c1"), arts, user),
		workerCall("worker-r0", at(2), call("c2"), arts, user, call("c1"), bad),
		workerCall("worker-r0", at(3), genai.NewContentFromText("draft", genai.RoleModel), arts, user, call("c1"), bad, call("c2"), good),
		evalScoreEntry("n1", "judge-r1", at(4), "citation_quality", 1.0/3),
		evalScoreEntry("n1", "judge-r1", at(4), "grounded", 0),
		workerCall("worker-r1", at(5), genai.NewContentFromText("the real answer", genai.RoleModel), arts, genai.NewContentFromText("revise", genai.RoleUser)),
		evalScoreEntry("n1", "judge-r2", at(6), "citation_quality", 1),
		evalScoreEntry("n1", "judge-r2", at(6), "cites_sources", 0.9),
	)
}

// fakeExperiment starts the experiment extension on a fake Dispatch that ends every run with status;
// onRun first gets the dispatched chat id, so a test can seed that chat's rows.
func fakeExperiment(t *testing.T, status extsdk.RunStatus, onRun func(chatID string)) *[]extsdk.DispatchRequest {
	t.Helper()
	var reqs []extsdk.DispatchRequest
	var ext *experimentExt
	dispatch := func(_ context.Context, req extsdk.DispatchRequest) error {
		reqs = append(reqs, req)
		chatID := "ext:experiment:" + req.Chat.LocalID
		if onRun != nil {
			onRun(chatID)
		}
		go ext.RunEnded(chatID, extsdk.RunOutcome{Status: status, Error: "boom"})
		return nil
	}
	e, _ := newExperimentExt(extsdk.Host{Dispatch: dispatch}, nil)
	ext = e.(*experimentExt)
	if err := ext.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { liveExperiment.Store(nil) })
	return &reqs
}

// fakeQuackAPI serves recording for every chat and fails the test on any other call: an
// experiment item must never create a chat or send a message the orchestrator would plan.
func fakeQuackAPI(t *testing.T, recording []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/chats/{chat}/recording", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(recording)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("experiment item reached %s %s; it must run the node directly", r.Method, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// EnableExperiment's workflow is a bound shape: one node of the agent whose task is the ask itself, which
// the bound dispatch runs with no orchestrator or planner turn (TestSDKExtensionDispatch_BoundWorkflowSkipsPlannerLLM).
func TestEnableExperiment_BindsOneNodeToTheAsk(t *testing.T) {
	cfg := &config.Config{}
	EnableExperiment(cfg, "synthesizer")
	if _, ok := cfg.Extensions.Modules["experiment"]; !ok {
		t.Fatal("experiment extension not configured")
	}
	shape, ok := workflowcatalog.Lookup(workflowcatalog.FromConfig(cfg.Workflows, ""), "experiment-synthesizer")
	if !ok {
		t.Fatal("no experiment-synthesizer workflow")
	}
	nodes, bound := workflowcatalog.Bind(shape, "the recorded task")
	if !bound || len(nodes) != 1 || nodes[0].Agent != "synthesizer" || nodes[0].Task != "the recorded task" {
		t.Fatalf("bound nodes = %+v (bound %v), want one synthesizer node whose task is the ask", nodes, bound)
	}
}

// TestLiveItemRunner_RunItem: an item dispatches the agent's bound workflow (never the orchestrator), with
// delivery denied; the run carries the resolved prompt, the final judge round on the rubric's scale, and the node's cost.
func TestLiveItemRunner_RunItem(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	reqs := fakeExperiment(t, extsdk.RunDone, func(chatID string) {
		node := store.DagNode{NodeID: "n1", PlanID: "plan1", TraceID: "trace-1", TraceIDSet: true, PromptTokens: 1200,
			CompletionTokens: 300, CachedTokens: 800, JudgeRounds: 2, DurationMs: 4500, JudgePassed: true}
		if err := st.SetChatOrigin(ctx, chatID, "u", ""); err != nil {
			t.Error(err)
		}
		if err := st.SaveDagPlan(ctx, chatID, "plan1", "turn1", "{}"); err != nil {
			t.Error(err)
		}
		if err := st.UpsertDagNode(ctx, node); err != nil {
			t.Error(err)
		}
	})
	srv := fakeQuackAPI(t, synthesizerRecording(t))

	rubric, err := vetting.LoadRubricScaler(ctx, nil, "agents/synthesizer")
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewLiveItemRunner(srv.URL, st, "synthesizer", rubric).RunItem(ctx, "synthesize this")
	if err != nil {
		t.Fatalf("RunItem: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(*reqs))
	}
	req := (*reqs)[0]
	if req.Run.Workflow != "experiment-synthesizer" || req.Ask.Message != "synthesize this" ||
		req.Delivery.AllowedKinds == nil || len(req.Delivery.AllowedKinds) != 0 {
		t.Fatalf("dispatch = %+v, want the bound synthesizer workflow on the task with delivery denied", req)
	}
	if run.Answer != "the real answer" || run.TraceID != "trace-1" || run.prompt() != "system/synthesizer@4" || !run.Passed {
		t.Fatalf("answer/trace/prompt/passed = %q/%q/%q/%v", run.Answer, run.TraceID, run.prompt(), run.Passed)
	}
	// Round 2 only (grounded was judged in round 1 alone), citation_quality back on its 0-3 scale.
	want := []criterionScore{{"citation_quality", 3, "citation_quality why"}, {"cites_sources", 0.9, "cites_sources why"}}
	if !reflect.DeepEqual(run.Criteria, want) {
		t.Fatalf("criteria = %+v, want %+v", run.Criteria, want)
	}
	wantCost := itemCost{Activity: bundle.Activity{ModelTurns: 4, ToolCalls: 2, RejectedToolCalls: 1},
		InputTokens: 1200, OutputTokens: 300, CachedTokens: 800, JudgeRounds: 2, WallTime: 4500 * time.Millisecond}
	if run.Cost != wantCost {
		t.Fatalf("cost = %+v, want %+v", run.Cost, wantCost)
	}
}

// A run that didn't end done, or a recording with no run for Agent, is an item error, never a run item.
func TestLiveItemRunner_RunItem_NoNodeRun(t *testing.T) {
	// A recording with no run for "code-reviewer" at all.
	srv := fakeQuackAPI(t, recordingBody(t, "c1", "n1", "synthesizer", "x", "y"))
	for _, status := range []extsdk.RunStatus{extsdk.RunDone, extsdk.RunFailed} {
		fakeExperiment(t, status, nil)
		r := NewLiveItemRunner(srv.URL, nil, "code-reviewer", vetting.RubricScaler{})
		if _, err := r.RunItem(context.Background(), "review this"); err == nil {
			t.Fatalf("%s: want an error when the agent produced no node run", status)
		}
	}
}

func TestLiveItemRunner_NodeRow(t *testing.T) {
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
	if got := r.nodeRow(ctx, chat.ID, "node-1"); got == nil || got.TraceID != "trace-real-1" {
		t.Fatalf("nodeRow = %+v, want the node's row with its trace id", got)
	}
	if got := r.nodeRow(ctx, chat.ID, "no-such-node"); got != nil {
		t.Fatalf("nodeRow for an unknown node = %+v, want nil", got)
	}
	if got := (&LiveItemRunner{}).nodeRow(ctx, chat.ID, "node-1"); got != nil {
		t.Fatalf("nodeRow with no Store = %+v, want nil", got)
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

// A node run whose trace id never landed must surface as an item error, not a run
// item that links to nothing.
func TestRunExperiment_EmptyTraceIDIsAnError(t *testing.T) {
	var runItems int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/public/dataset-items":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item1", "input": map[string]any{"task": "x"}}},
				"meta": map[string]any{"page": 1, "limit": 1, "totalItems": 1, "totalPages": 1}})
		case r.URL.Path == "/api/public/dataset-run-items":
			runItems++
			writeJSON(w, map[string]any{"id": "ri"})
		}
	}))
	defer srv.Close()
	lf := newTestClient(t, srv)

	res, err := RunExperiment(context.Background(), io.Discard, &stubRunner{}, lf, ExperimentOpts{Dataset: "my-dataset"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Error == "" || runItems != 0 {
		t.Fatalf("results=%+v runItems=%d, want one errored result and no run item", res, runItems)
	}
}

// fakeExperimentLangfuse serves one dataset item per task and records every ingestion
// batch's events and run item body it receives.
func fakeExperimentLangfuse(t *testing.T, tasks ...string) (*httptest.Server, *[]map[string]any, *[]map[string]any) {
	t.Helper()
	var events, runItems []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/public/dataset-items":
			var data []map[string]any
			for i, task := range tasks {
				data = append(data, map[string]any{"id": fmt.Sprintf("item%d", i+1), "input": map[string]any{"task": task}})
			}
			writeJSON(w, map[string]any{"data": data, "meta": map[string]any{"totalPages": 1}})
		case "/api/public/ingestion":
			var body struct{ Batch []map[string]any }
			_ = json.NewDecoder(r.Body).Decode(&body)
			events = append(events, body.Batch...)
			ingestOK(w)
		case "/api/public/dataset-run-items":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			runItems = append(runItems, body)
			writeJSON(w, map[string]any{"id": "ri"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &events, &runItems
}

// Each item's trace gets one numeric score per judged criterion (rubric scale, judge's reason as comment),
// gate_passed and the cost.* signals; its run item carries the prompt the node actually ran.
func TestRunExperiment_ScoresTraceAndLinksPrompt(t *testing.T) {
	srv, events, runItems := fakeExperimentLangfuse(t, "synthesize this")
	run := itemRun{
		Prompt:   ledger.ArtifactRef{Name: "system/synthesizer", Source: "langfuse", VersionID: "4"},
		Criteria: []criterionScore{{"citation_quality", 2, "one late cite"}, {"cites_sources", 0.9, ""}},
		Passed:   true,
		Cost: itemCost{Activity: bundle.Activity{ModelTurns: 6, ToolCalls: 5, RejectedToolCalls: 1},
			InputTokens: 1200, OutputTokens: 300, CachedTokens: 800, JudgeRounds: 2, WallTime: 4500 * time.Millisecond},
	}
	_, err := RunExperiment(context.Background(), io.Discard, &stubRunner{traceID: "tr1", run: run}, newTestClient(t, srv),
		ExperimentOpts{Dataset: "ds", Agent: "synthesizer", RunName: "baseline"})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]float64{"citation_quality": 2, "cites_sources": 0.9, "gate_passed": 1,
		"cost.input_tokens": 1200, "cost.output_tokens": 300, "cost.cached_tokens": 800, "cost.model_turns": 6,
		"cost.tool_calls": 5, "cost.rejected_tool_calls": 1, "cost.judge_rounds": 2, "cost.wall_time_s": 4.5}
	got := map[string]float64{}
	for _, e := range *events {
		b := e["body"].(map[string]any)
		name := b["name"].(string)
		if e["type"] != "score-create" || b["traceId"] != "tr1" || b["dataType"] != "NUMERIC" || b["id"] != "tr1-"+name || e["id"] != b["id"] {
			t.Errorf("score %s: event %+v", name, e)
		}
		got[name] = b["value"].(float64)
		if name == "citation_quality" && b["comment"] != "one late cite" {
			t.Errorf("citation_quality comment = %v, want the judge's explanation", b["comment"])
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scores = %v, want %v", got, want)
	}

	if len(*runItems) != 1 {
		t.Fatalf("run items = %+v, want one", *runItems)
	}
	ri := (*runItems)[0]
	wantMeta := map[string]any{"agent": "synthesizer", "prompt_name": "system/synthesizer", "prompt_version": "4", "prompt_source": "langfuse"}
	if ri["traceId"] != "tr1" || ri["runName"] != "baseline" || !reflect.DeepEqual(ri["metadata"], wantMeta) ||
		ri["runDescription"] != "prompt=system/synthesizer@4 agent=synthesizer" {
		t.Fatalf("run item = %+v", ri)
	}
}

// Langfuse answers a batch 207 even when events fail. A rejected score fails the run, naming the event and
// its error, and the item is never linked into the dataset run with scores missing.
func TestRunExperiment_RejectedScoreFailsLoudly(t *testing.T) {
	runItems := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/public/dataset-items":
			writeJSON(w, map[string]any{"data": []map[string]any{{"id": "item1", "input": map[string]any{"task": "x"}}},
				"meta": map[string]any{"totalPages": 1}})
		case "/api/public/ingestion":
			w.WriteHeader(http.StatusMultiStatus)
			writeJSON(w, map[string]any{"successes": []any{}, "errors": []map[string]any{
				{"id": "tr1-gate_passed", "status": 400, "message": "invalid score value"}}})
		case "/api/public/dataset-run-items":
			runItems++
			writeJSON(w, map[string]any{"id": "ri"})
		}
	}))
	defer srv.Close()

	_, err := RunExperiment(context.Background(), io.Discard, &stubRunner{traceID: "tr1"}, newTestClient(t, srv),
		ExperimentOpts{Dataset: "ds", Agent: "synthesizer", RunName: "rig"})
	if err == nil || !strings.Contains(err.Error(), "tr1-gate_passed") || !strings.Contains(err.Error(), "invalid score value") {
		t.Fatalf("err = %v, want a failure naming the rejected score and its error", err)
	}
	if runItems != 0 {
		t.Fatalf("run items = %d, want none for an item whose scores were rejected", runItems)
	}
}

// A --prompt pin the node never resolved (a static fallback, or no system/<agent> artifact at all)
// is an item error: scoring it would credit the pinned version with another prompt's results.
func TestRunExperiment_PinNotRunIsAnItemError(t *testing.T) {
	for name, ref := range map[string]ledger.ArtifactRef{
		"static fallback": {Name: "system/synthesizer", Source: "static", VersionID: "4"},
		"other version":   {Name: "system/synthesizer", Source: "langfuse", VersionID: "3"},
		"no artifact":     {},
	} {
		t.Run(name, func(t *testing.T) {
			srv, events, runItems := fakeExperimentLangfuse(t, "x")
			res, err := RunExperiment(context.Background(), io.Discard, &stubRunner{traceID: "tr1", run: itemRun{Prompt: ref}},
				newTestClient(t, srv), ExperimentOpts{Dataset: "ds", Agent: "synthesizer", Prompt: "system/synthesizer@4"})
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || !strings.Contains(res[0].Error, "pinned system/synthesizer@4") || len(*events)+len(*runItems) != 0 {
				t.Fatalf("results=%+v events=%d runItems=%d, want an item error and nothing sent", res, len(*events), len(*runItems))
			}
		})
	}
}
