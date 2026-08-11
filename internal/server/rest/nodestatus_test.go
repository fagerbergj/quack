package rest

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// stubModel is a minimal model.LLM that always answers with a plain text
// reply and no tool calls, so the orchestrator's llmagent completes without
// going through plan/execute - enough to exercise SendChatMessage end to end
// without a real research run.
type stubModel struct{}

func (stubModel) Name() string { return "stub" }

func (stubModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "hi"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// newTestHandler builds a Handler backed by a real (sqlite, temp-file)
// store and a real Orchestrator/Executor with an empty agent roster and a
// stub top-level model - enough to run CancelNode/SteerNode/RetryNode (which
// never reach a real gated node in these tests) and a full SendChatMessage
// direct-answer turn (no plan/execute).
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	return newTestHandlerWithModel(t, stubModel{})
}

// newTestHandlerWithModel is newTestHandler with an injectable top-level model
// (e.g. a request-capturing stub for conversation-memory tests).
func newTestHandlerWithModel(t *testing.T, m model.LLM) *Handler {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{}, map[string]model.LLM{}, nil,
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	orch := orchestrator.New(st.Sessions, m, "You are a test duck.", planner, ex, nil, nil, nil)
	artifacts, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifacts)
	return NewHandler(st, orch, nil, nil, nil, nil, "test", nil, nil, store.NewTurnAwareService(artifacts))
}

// mustCreateChat inserts a real chat row and returns its (store-minted) id -
// SendChatMessage/SubscribeChatStream 404 preflight-check chat existence now,
// so any test driving them needs a real row, not an arbitrary literal id.
func mustCreateChat(t *testing.T, h *Handler) string {
	t.Helper()
	c, err := h.store.CreateChat(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return c.ID
}

// --- UpdateNodeStatus -------------------------------------------------------

// seedPlan writes a minimal DagPlan (+ optional DagNode) fixture directly to
// the store, standing in for a completed orchestrator run.
func seedPlan(t *testing.T, h *Handler, chatID, planID, nodeID string) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.SaveTurn(ctx, chatID, "turn-"+planID); err != nil {
		t.Fatalf("SaveTurn: %v", err)
	}
	planJSON, _ := json.Marshal(map[string]any{
		"plan_id": planID,
		"nodes":   []map[string]any{{"id": nodeID, "agent": "a", "task": "t", "depends_on": []string{}}},
		"edges":   []map[string]any{},
	})
	if err := h.store.SaveDagPlan(ctx, chatID, planID, "turn-"+planID, string(planJSON)); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
}

func putNodeStatus(t *testing.T, h *Handler, chatID, nodeID string, body schema.NodeStatusUpdateBody) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/status", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.UpdateNodeStatus(rec, req, chatID, nodeID)
	return rec
}

// TestUpdateNodeStatus_CancelUndeliverable409: cancel, like steer, is NOT
// optimistic. The node's persisted row says "running", but with no live control
// registered the cancel lands nowhere - and the old handler discarded
// CancelNode's bool and answered 200 + "cancelled" anyway. Live (2026-07-13) the
// user hit Cancel six times in one second, got six 200s, and the node ran on:
// "cancel and steer is seemingly doing nothing". Delivery success is exercised at
// the dag layer (control tests) and live e2e.
func TestUpdateNodeStatus_CancelUndeliverable409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusCancelled})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (nothing live to cancel); body=%s", rec.Code, rec.Body.String())
	}
	var got schema.TransitionError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(got.Error, "not cancellable") {
		t.Errorf("409 body should explain nothing was cancelled; got %q", got.Error)
	}
	if got.Current != schema.NodeStatusRunning {
		t.Errorf("Current = %q, want %q (the node is still running - nothing changed)", got.Current, schema.NodeStatusRunning)
	}
}

func TestUpdateNodeStatus_IllegalTransition409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "done"}); err != nil {
		t.Fatalf("seed done node: %v", err)
	}

	// done → needs_input is illegal (done only legally re-queues via retry) and
	// needs no guidance, so it isolates the 409 transition check from the 400
	// guidance-required check (covered separately below).
	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusNeedsInput})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var got schema.TransitionError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Current != schema.NodeStatusDone {
		t.Errorf("Current = %q, want %q", got.Current, schema.NodeStatusDone)
	}
	found := false
	for _, a := range got.Allowed {
		if a == schema.NodeStatusQueued {
			found = true
		}
	}
	if !found {
		t.Errorf("Allowed = %v, want it to include %q (retry)", got.Allowed, schema.NodeStatusQueued)
	}
}

func TestUpdateNodeStatus_CancelledToNeedsInput409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "cancelled"}); err != nil {
		t.Fatalf("seed cancelled node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusNeedsInput})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateNodeStatus_PauseUndeliverable409: pause, like cancel, is NOT
// optimistic - the node's persisted row says "running", but with no live
// control registered the pause lands nowhere.
func TestUpdateNodeStatus_PauseUndeliverable409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusPaused})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (nothing live to pause); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not pausable") {
		t.Errorf("409 body should explain nothing was paused; body=%s", rec.Body.String())
	}
}

// TestUpdateNodeStatus_ResumePausedNode: paused → running (resume) is legal
// and kicks off a re-run the same way retry does (optimistic "queued").
func TestUpdateNodeStatus_ResumePausedNode(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "paused"}); err != nil {
		t.Fatalf("seed paused node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got schema.DagNodeState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != schema.NodeStatusQueued {
		t.Errorf("Status = %q, want %q (optimistic resume re-run)", got.Status, schema.NodeStatusQueued)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestUpdateNodeStatus_RunningSelfLoopIllegal: the old steer-via-status
// (running → running) no longer exists - steering is queueing a message
// (POST .../queue), which doesn't transition the node's status at all.
func TestUpdateNodeStatus_RunningSelfLoopIllegal(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (running -> running is no longer legal); body=%s", rec.Code, rec.Body.String())
	}
}

func putQueueMessage(t *testing.T, h *Handler, chatID, nodeID string, body schema.QueueMessageBody) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/queue", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.QueueNodeMessage(rec, req, chatID, nodeID)
	return rec
}

// TestQueueNodeMessage_NoLiveNode404: a message aimed at a node with no live
// control (not currently running) has nowhere to land.
func TestQueueNodeMessage_NoLiveNode404(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	rec := putQueueMessage(t, h, chatID, nodeID, schema.QueueMessageBody{Message: "focus on cost"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no live control to deliver to); body=%s", rec.Code, rec.Body.String())
	}
}

func TestQueueNodeMessage_EmptyMessage400(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)

	rec := putQueueMessage(t, h, chatID, nodeID, schema.QueueMessageBody{Message: "  "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestEditNodeTask_NotStartedOK_StartedConflict: a not-yet-started node's task
// can be edited; once "started" (its control is live) it's immutable.
func TestEditNodeTask_NotStartedOK_StartedConflict(t *testing.T) {
	h := newTestHandler(t)
	chatID, nodeID := "c1", "n1"

	body, _ := json.Marshal(schema.EditNodeTaskBody{Task: "revised task"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/chats/"+chatID+"/nodes/"+nodeID, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.EditNodeTask(rec, req, chatID, nodeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (node hasn't started); body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateNodeStatus_RetryFailedNode(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "failed", Error: "boom"}); err != nil {
		t.Fatalf("seed failed node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusQueued})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got schema.DagNodeState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != schema.NodeStatusQueued {
		t.Errorf("Status = %q, want %q (optimistic re-queue)", got.Status, schema.NodeStatusQueued)
	}
	// The retry itself runs in the background; give it a moment to at least
	// reach its "no plan in session to retry" error path without panicking.
	time.Sleep(50 * time.Millisecond)
}

func TestUpdateNodeStatus_NoSuchNode404(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)

	rec := putNodeStatus(t, h, chatID, "does-not-exist", schema.NodeStatusUpdateBody{Status: schema.NodeStatusCancelled})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateNodeStatus_NoPlan404(t *testing.T) {
	h := newTestHandler(t)
	rec := putNodeStatus(t, h, "no-such-chat", "n1", schema.NodeStatusUpdateBody{Status: schema.NodeStatusCancelled})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- UpdateResponseStatus ----------------------------------------------------

func TestUpdateResponseStatus_CancelsActiveRun(t *testing.T) {
	h := newTestHandler(t)
	chatID, responseID := "c1", "r1"
	cancelled := false
	h.hub.RegisterRun(chatID, responseID, func() { cancelled = true })

	b, _ := json.Marshal(schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/chats/"+chatID+"/responses/"+responseID+"/status", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.UpdateResponseStatus(rec, req, chatID, responseID)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !cancelled {
		t.Error("cancel func was not invoked")
	}
}

func TestUpdateResponseStatus_WrongResponseID404(t *testing.T) {
	h := newTestHandler(t)
	chatID := "c1"
	cancelled := false
	h.hub.RegisterRun(chatID, "the-real-one", func() { cancelled = true })

	b, _ := json.Marshal(schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/chats/"+chatID+"/responses/stale/status", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.UpdateResponseStatus(rec, req, chatID, "stale")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if cancelled {
		t.Error("cancel func should not have been invoked for a stale response id")
	}
}

// --- DeleteChat --------------------------------------------------------------

// TestDeleteChat_CancelsActiveRun is #468's core regression: DELETE must kill
// a run still in flight on the chat, not just drop its row while the run
// keeps executing. Registers a run's cancel handle the same way startRun does
// (via the shared hub) and asserts DeleteChat invokes it.
func TestDeleteChat_CancelsActiveRun(t *testing.T) {
	h := newTestHandler(t)
	chatID := "c1"
	if _, err := h.store.CreateChat(context.Background(), ""); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	cancelled := false
	h.hub.RegisterRun(chatID, "r1", func() { cancelled = true })

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/chats/"+chatID, nil)
	rec := httptest.NewRecorder()
	h.DeleteChat(rec, req, chatID)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !cancelled {
		t.Error("DeleteChat did not cancel the chat's active run")
	}
}

// TestDeleteChat_UnknownOrFinishedChatNoOp confirms DELETE stays a safe no-op
// (still 204, no panic) when nothing is registered - the already-finished or
// never-started case.
func TestDeleteChat_UnknownOrFinishedChatNoOp(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/chats/no-such-chat", nil)
	rec := httptest.NewRecorder()
	h.DeleteChat(rec, req, "no-such-chat")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateResponseStatus_NoActiveRun404(t *testing.T) {
	h := newTestHandler(t)
	b, _ := json.Marshal(schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/chats/c1/responses/r1/status", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.UpdateResponseStatus(rec, req, "c1", "r1")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- response_created is the first event of a run --------------------------

// TestSendChatMessage_ResponseCreatedFirst runs a full (stubbed) turn and
// checks that response_created is the very first SSE event, carrying the same
// id as the chat's persisted turn - and that the response is no longer
// cancellable by that id once the run (and handler call) has returned.
func TestSendChatMessage_ResponseCreatedFirst(t *testing.T) {
	h := newTestHandler(t)
	chatID := mustCreateChat(t, h)

	body := `{"content":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.SendChatMessage(rec, req, chatID)

	events := parseSSEBody(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no SSE events received")
	}
	if events[0].name != "response_created" {
		t.Fatalf("first event = %q, want response_created (got: %v)", events[0].name, eventNames(events))
	}
	var d struct {
		ResponseID string `json:"response_id"`
	}
	if err := json.Unmarshal([]byte(events[0].data), &d); err != nil {
		t.Fatalf("decode response_created data: %v", err)
	}
	if d.ResponseID == "" {
		t.Fatal("response_created carried an empty response_id")
	}

	// The run has already returned (SendChatMessage is synchronous in this
	// test), so activeCancels was cleared - cancelling by that id now 404s.
	b, _ := json.Marshal(schema.ResponseStatusUpdateBody{Status: schema.Cancelled})
	req2 := httptest.NewRequest(http.MethodPut, "/api/v1/chats/"+chatID+"/responses/"+d.ResponseID+"/status", strings.NewReader(string(b)))
	rec2 := httptest.NewRecorder()
	h.UpdateResponseStatus(rec2, req2, chatID, d.ResponseID)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("cancel after run end: status = %d, want 404", rec2.Code)
	}
}

type sseEvent struct{ name, data string }

func eventNames(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.name
	}
	return out
}

// parseSSEBody splits a raw SSE response body into (name,data) pairs.
func parseSSEBody(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	var name string
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			out = append(out, sseEvent{name: name, data: strings.TrimPrefix(line, "data: ")})
		}
	}
	return out
}

// --- needs_input persistence -------------------------------------------------

// TestNeedsInputPersistsAcrossReload: a HITL pause (node_needs_input) persists
// the node's status as "needs_input" - visible in the turn's quack:dag output
// item after a simulated reload (GetTurnsWithContent → buildTurn), not just in
// the live SSE stream. The DAG item itself reads in_progress while any node is
// still needs_input (a paused run is not "completed").
func TestNeedsInputPersistsAcrossReload(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)

	// A real HITL pause always follows node_start (running); persist that first
	// and wait for it to land so the needs_input write below is a legal
	// running → needs_input transition, not queued → needs_input.
	runlog.PersistNodeEvent(h.store, planID, stream.NodeStart(nodeID, "a"))
	waitForDagNodeStatus(t, h, planID, nodeID, "running")

	runlog.PersistNodeEvent(h.store, planID, stream.NodeNeedsInput(nodeID, "int-1", "which region?"))
	waitForDagNodeStatus(t, h, planID, nodeID, "needs_input")

	turns, err := h.store.GetTurnsWithContent(ctx, orchestrator.AppName, userID, chatID)
	if err != nil {
		t.Fatalf("GetTurnsWithContent: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(turns))
	}
	turn := buildTurn(turns[0])

	var dagItem *schema.DagOutputItem
	for _, item := range turn.Output {
		if disc, _ := item.Discriminator(); disc == "quack:dag" {
			d, err := item.AsDagOutputItem()
			if err != nil {
				t.Fatalf("AsDagOutputItem: %v", err)
			}
			dagItem = &d
		}
	}
	if dagItem == nil {
		t.Fatal("no quack:dag output item after reload")
	}
	ns, ok := dagItem.NodeStates[nodeID]
	if !ok {
		t.Fatalf("node %q missing from node_states: %v", nodeID, dagItem.NodeStates)
	}
	if ns.Status != schema.NodeStatusNeedsInput {
		t.Errorf("node status = %q, want %q", ns.Status, schema.NodeStatusNeedsInput)
	}
	if dagItem.Status != schema.InProgress {
		t.Errorf("dag status = %q, want %q (a paused run isn't completed)", dagItem.Status, schema.InProgress)
	}
}

// waitForDagNodeStatus polls the store for a node's persisted status - writes
// go through an internal goroutine (persistNodeEvent is fire-and-forget).
func waitForDagNodeStatus(t *testing.T, h *Handler, planID, nodeID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := h.store.GetDagNode(context.Background(), planID, nodeID)
		if err == nil && n != nil && n.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node %q in plan %q never reached status %q", nodeID, planID, want)
}
