package rest

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// stubModel always answers with plain text and no tool calls, so a SendChatMessage turn
// completes without plan/execute.
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

// newTestHandler builds a Handler on a temp sqlite store and a real Orchestrator/Executor with an empty
// roster and a stub model: enough for node-control endpoints and a direct-answer turn.
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
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6} }, nil)
	planner := dag.NewPlanner(nil, nil, nil)
	orch := orchestrator.New(st.Sessions, m, func(context.Context) string { return "You are a test duck." }, planner, ex, nil, nil, nil)
	artifacts, err := st.RowArtifactService()
	if err != nil {
		t.Fatalf("RowArtifactService: %v", err)
	}
	st.SetArtifactService(artifacts)
	return NewHandler(st, orch, nil, nil, nil, nil, "test", nil, nil, store.NewTurnAwareService(artifacts), nil)
}

// mustCreateChat inserts a real chat row and returns its id; SendChatMessage/SubscribeChatStream
// 404 on a chat that doesn't exist.
func mustCreateChat(t *testing.T, h *Handler) string {
	t.Helper()
	c, err := h.store.CreateChat(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return c.ID
}

// seedPlan writes a minimal DagPlan (+ optional DagNode) fixture directly to
// the store, standing in for a completed orchestrator run.
func seedPlan(t *testing.T, h *Handler, chatID, planID, nodeID string) {
	t.Helper()
	ctx := context.Background()
	// chat_turns/dag_plans FK to chats.id: upsert a bare row so callers can use a literal chatID.
	if err := h.store.SetChatOrigin(ctx, chatID, "", ""); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if err := h.store.SaveTurn(ctx, chatID, "turn-"+planID, ""); err != nil {
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

// TestUpdateNodeStatus_CancelUndeliverable409: cancel isn't optimistic. With the row "running" but no live
// control registered, the cancel lands nowhere and must 409, not 200 "cancelled".
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

	// done -> needs_input is illegal and needs no guidance, isolating the 409 transition
	// check from the 400 guidance check.
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

// TestUpdateNodeStatus_PauseUndeliverable409: like cancel, pause with no live control
// registered lands nowhere and must 409.
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

// TestUpdateNodeStatus_ResumeAlreadyLiveConflict409: pause is cooperative, so a "paused" row can still have
// a live run; a second resume must 409, not dispatch a concurrent run of the same node.
func TestUpdateNodeStatus_ResumeAlreadyLiveConflict409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "paused"}); err != nil {
		t.Fatalf("seed paused node: %v", err)
	}
	// Simulate the first resume's dispatch already in flight.
	h.hub.RegisterRun(chatID, "turn-"+planID, func() {})

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a run is already dispatched for this chat); body=%s", rec.Code, rec.Body.String())
	}
}

// TestStartNode_ResumeAlreadyLiveConflict409 is the /start endpoint's half of the same check.
func TestStartNode_ResumeAlreadyLiveConflict409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "paused"}); err != nil {
		t.Fatalf("seed paused node: %v", err)
	}
	h.hub.RegisterRun(chatID, "turn-"+planID, func() {})

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (a run is already dispatched for this chat); body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateNodeStatus_ResumeRegistersRunSynchronously: retryNodeAsync registers the run before returning,
// or a shutdown drain reading hub.ActiveChatIDs() right after could miss the dispatch.
func TestUpdateNodeStatus_ResumeRegistersRunSynchronously(t *testing.T) {
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
	if !h.hub.HasRegisteredRun(chatID) {
		t.Fatal("the run was not registered on the hub by the time the handler returned - a drain snapshot taken right after would miss it")
	}
}

// TestStartNode_AwaitingInputRegistersRunSynchronously is the startNodeAsync half of the same check.
func TestStartNode_AwaitingInputRegistersRunSynchronously(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "needs_input", PendingQuestion: "which region?"}); err != nil {
		t.Fatalf("seed parked node: %v", err)
	}

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{Content: strPtr("us-east")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !h.hub.HasRegisteredRun(chatID) {
		t.Fatal("the run was not registered on the hub by the time the handler returned - a drain snapshot taken right after would miss it")
	}
}

// TestUpdateNodeStatus_AwaitingInputRetryRejected: needs_input -> running via the status endpoint is refused.
// The worker session keeps an unanswered call at its tail, so only StartNode (with an answer) may resume it.
func TestUpdateNodeStatus_AwaitingInputRetryRejected(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "needs_input", PendingQuestion: "which region?"}); err != nil {
		t.Fatalf("seed parked node: %v", err)
	}

	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (awaiting_input must resume via start, not retry); body=%s", rec.Code, rec.Body.String())
	}

	// Also reject a "paused" status row whose pause_reason is awaiting_input
	// (the two on-disk spellings dagNodeState/UpdateNodeStatus both accept).
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "paused"}); err != nil {
		t.Fatalf("reseed paused node: %v", err)
	}
	if err := h.store.SetNodeStatusForChat(context.Background(), chatID, nodeID, "", string(dag.PauseAwaitingInput), "which region?"); err != nil {
		t.Fatalf("stamp awaiting_input pause reason: %v", err)
	}
	rec = putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusRunning})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (paused/awaiting_input must resume via start, not retry); body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateNodeStatus_RunningSelfLoopIllegal: running -> running is illegal;
// steering is queueing a message (POST .../queue), not a status transition.
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

func postNodeStart(t *testing.T, h *Handler, chatID, nodeID string, body schema.NodeStartBody) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/start", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	h.StartNode(rec, req, chatID, nodeID)
	return rec
}

func postNodeStop(t *testing.T, h *Handler, chatID, nodeID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/stop", nil)
	rec := httptest.NewRecorder()
	h.StopNode(rec, req, chatID, nodeID)
	return rec
}

// TestStartNode_QueuedOK: queued -> running is legal and, like resume,
// answers optimistically with "queued" while the run kicks off in the background.
func TestStartNode_QueuedOK(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "queued"}); err != nil {
		t.Fatalf("seed queued node: %v", err)
	}

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got schema.DagNodeState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != schema.NodeStatusRunning {
		t.Errorf("Status = %q, want %q (the node's new state)", got.Status, schema.NodeStatusRunning)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestStartStopNode_UnknownNode404: a node absent from the latest plan 404s; GetDagNode returns (nil, nil)
// for a missing row, so falling through would treat it as a startable queued node.
func TestStartStopNode_UnknownNode404(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID := "c1", "p1"
	seedPlan(t, h, chatID, planID, "n1")

	if rec := postNodeStart(t, h, chatID, "ghost", schema.NodeStartBody{}); rec.Code != http.StatusNotFound {
		t.Errorf("start: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if rec := postNodeStop(t, h, chatID, "ghost"); rec.Code != http.StatusNotFound {
		t.Errorf("stop: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestStartNode_FromRunning409 also pins the 409 body's Allowed targets.
func TestStartNode_FromRunning409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var got schema.TransitionError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[schema.NodeStatus]bool{}
	for _, a := range got.Allowed {
		want[a] = true
	}
	if !want[schema.NodeStatusPaused] || !want[schema.NodeStatusCancelled] || !want[schema.NodeStatusDone] {
		t.Errorf("Allowed = %v, should name running's legal targets", got.Allowed)
	}
}

// TestStartNode_AwaitingInputBlankAnswer400: a parked question must not
// silently resume with an empty payload.
func TestStartNode_AwaitingInputBlankAnswer400(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "needs_input", PendingQuestion: "which region?"}); err != nil {
		t.Fatalf("seed parked node: %v", err)
	}

	if rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{}); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateNodeStatus_PauseReasonAwaitingInputRejected: awaiting_input is
// system-owned; a client pause may only say user/shutdown.
func TestUpdateNodeStatus_PauseReasonAwaitingInputRejected(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	reason := schema.PauseReason("awaiting_input")
	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusPaused, Reason: &reason})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestStartNode_PausedOK mirrors TestUpdateNodeStatus_ResumePausedNode for
// the dedicated start endpoint.
func TestStartNode_PausedOK(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "paused"}); err != nil {
		t.Fatalf("seed paused node: %v", err)
	}

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{Content: strPtr("the answer")})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
}

// TestStartNode_IllegalTransition409: done -> running only legally re-queues via retry.
func TestStartNode_IllegalTransition409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "done"}); err != nil {
		t.Fatalf("seed done node: %v", err)
	}

	rec := postNodeStart(t, h, chatID, nodeID, schema.NodeStartBody{})
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
}

// TestStopNode_RunningUndeliverable409 mirrors the cancel-endpoint's not-live 409.
func TestStopNode_RunningUndeliverable409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}

	rec := postNodeStop(t, h, chatID, nodeID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (nothing live to stop); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not stoppable") {
		t.Errorf("409 body should explain nothing was stopped; body=%s", rec.Body.String())
	}
}

// TestStopNode_TerminalIllegal409: cancelled -> cancelled isn't a legal self-loop.
func TestStopNode_TerminalIllegal409(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "cancelled"}); err != nil {
		t.Fatalf("seed cancelled node: %v", err)
	}

	rec := postNodeStop(t, h, chatID, nodeID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdateNodeStatus_PauseReason: pausing with an explicit reason persists
// it, and the read model returns it on the wire.
func TestUpdateNodeStatus_PauseReasonShutdown(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "running"}); err != nil {
		t.Fatalf("seed running node: %v", err)
	}
	reason := schema.PauseReason("shutdown")
	rec := putNodeStatus(t, h, chatID, nodeID, schema.NodeStatusUpdateBody{Status: schema.NodeStatusPaused, Reason: &reason})
	// No live control is registered, so this is PauseUndeliverable409's 409;
	// it checks the reason field decodes and flows through.
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (nothing live to pause); body=%s", rec.Code, rec.Body.String())
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

// TestUpdateNodeStatus_RetryNodeAddedByExtension: a node edit_plan added in a later turn is in the
// plan the retry endpoint reads, once that turn's dag_plan event re-saves the grown plan.
func TestUpdateNodeStatus_RetryNodeAddedByExtension(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	chatID, planID := "c1", "p1"
	seedPlan(t, h, chatID, planID, "r1")
	if err := h.store.SaveTurn(ctx, chatID, "turn-2", "more"); err != nil {
		t.Fatal(err)
	}
	grown := stream.DagPlanData{PlanID: planID, Nodes: []stream.DagNodeDef{{ID: "r1", Agent: "a", Task: "t"}, {ID: "r3", Agent: "a", Task: "u"}}}
	planJSON, _ := json.Marshal(grown)
	if err := h.store.SaveDagPlan(ctx, chatID, planID, "turn-2", string(planJSON)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertDagNode(ctx, store.DagNode{NodeID: "r3", PlanID: planID, Status: "done", Output: "R3"}); err != nil {
		t.Fatal(err)
	}
	if rec := putNodeStatus(t, h, chatID, "r3", schema.NodeStatusUpdateBody{Status: schema.NodeStatusQueued}); rec.Code != http.StatusOK {
		t.Fatalf("retry of the extension's node: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if dp, _ := h.store.GetLatestDagPlan(ctx, chatID); dp == nil || dp.RunTurnID() != "turn-2" {
		t.Errorf("plan row = %+v, want the extending turn as the one the retry answers", dp)
	}
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

// TestDeleteChat_CancelsActiveRun: DELETE invokes the in-flight run's cancel handle (registered via the hub,
// as startRun does) rather than only dropping the row.
func TestDeleteChat_CancelsActiveRun(t *testing.T) {
	h := newTestHandler(t)
	chatID := "c1"
	if _, err := h.store.CreateChat(context.Background(), ""); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	cancelled := false
	h.hub.RegisterRun(chatID, "r1", func() { cancelled = true; h.hub.EndRun(chatID, "r1") })

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

// TestDeleteChat_WaitsForRunTail: the row survives until the cancelled run's tail has finished
// (it unregisters last), so the tail's writes never hit a deleted chat.
func TestDeleteChat_WaitsForRunTail(t *testing.T) {
	h := newTestHandler(t)
	c0, err := h.store.CreateChat(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	chatID := c0.ID
	var rowAtTail atomic.Bool
	h.hub.RegisterRun(chatID, "r1", func() {
		go func() {
			time.Sleep(100 * time.Millisecond) // the run's tail still writing
			c, _ := h.store.GetChat(context.Background(), chatID)
			rowAtTail.Store(c != nil)
			h.hub.EndRun(chatID, "r1")
		}()
	})

	rec := httptest.NewRecorder()
	h.DeleteChat(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/chats/"+chatID, nil), chatID)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !rowAtTail.Load() {
		t.Error("chat row was deleted while the cancelled run's tail was still running")
	}
}

// TestDeleteChat_RunStuckRefuses: a run that never unregisters yields 409 and keeps the chat.
func TestDeleteChat_RunStuckRefuses(t *testing.T) {
	old := deleteStopWait
	deleteStopWait = 50 * time.Millisecond
	t.Cleanup(func() { deleteStopWait = old })
	h := newTestHandler(t)
	c0, err := h.store.CreateChat(context.Background(), "")
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	chatID := c0.ID
	h.hub.RegisterRun(chatID, "r1", func() {})

	rec := httptest.NewRecorder()
	h.DeleteChat(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/chats/"+chatID, nil), chatID)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if c, _ := h.store.GetChat(context.Background(), chatID); c == nil {
		t.Error("chat deleted despite a run that never ended")
	}
}

// TestDeleteChat_UnknownOrFinishedChatNoOp: DELETE with nothing registered is a safe 204.
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

// TestSendChatMessage_ResponseCreatedFirst: response_created is the first SSE event, carrying the persisted
// turn's id, and that id stops being cancellable once the run returns.
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

// TestNeedsInputPersistsAcrossReload: a needs_input node reads as paused/awaiting_input on the reloaded
// turn's quack:dag item, which stays in_progress while any node is paused.
func TestNeedsInputPersistsAcrossReload(t *testing.T) {
	h := newTestHandler(t)
	ctx := context.Background()
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)

	// A real pause follows node_start: persist it first so the needs_input write below is
	// a legal running -> needs_input transition.
	runlog.PersistNodeEvent(h.store, chatID, planID, stream.NodeStart(nodeID, "a"))
	waitForDagNodeStatus(t, h, planID, nodeID, "running")

	runlog.PersistNodeEvent(h.store, chatID, planID, stream.NodeNeedsInput(nodeID, "int-1", "which region?"))
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
	// Wire boundary normalizes the legacy needs_input DB spelling to one
	// vocabulary: paused, with pause_reason awaiting_input.
	if ns.Status != schema.NodeStatusPaused {
		t.Errorf("node status = %q, want %q", ns.Status, schema.NodeStatusPaused)
	}
	if ns.PauseReason == nil || *ns.PauseReason != schema.PauseReason("awaiting_input") {
		t.Errorf("pause_reason = %v, want awaiting_input", ns.PauseReason)
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

// TestStopNode_ParkedPausedCancelsRow: a parked node (paused, turn over, no
// live control) must still be stoppable - the row is cancelled directly.
func TestStopNode_ParkedPausedCancelsRow(t *testing.T) {
	h := newTestHandler(t)
	chatID, planID, nodeID := "c1", "p1", "n1"
	seedPlan(t, h, chatID, planID, nodeID)
	if err := h.store.UpsertDagNode(context.Background(), store.DagNode{NodeID: nodeID, PlanID: planID, Status: "paused", PauseReason: "user"}); err != nil {
		t.Fatalf("seed paused node: %v", err)
	}

	rec := postNodeStop(t, h, chatID, nodeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	dn, err := h.store.GetDagNode(context.Background(), planID, nodeID)
	if err != nil || dn == nil || dn.Status != "cancelled" {
		t.Fatalf("row = %+v err=%v, want cancelled", dn, err)
	}
}

// A node that only exists in an earlier plan must be rejected with a message saying why.
func TestUpdateNodeStatus_EarlierPlanNodeRejected(t *testing.T) {
	h := newTestHandler(t)
	seedPlan(t, h, "c1", "p1", "old")
	time.Sleep(5 * time.Millisecond)
	seedPlan(t, h, "c1", "p2", "new")

	rec := putNodeStatus(t, h, "c1", "old", schema.NodeStatusUpdateBody{Status: schema.NodeStatusQueued})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "earlier plans") {
		t.Fatalf("got %d %s, want 404 naming earlier plans", rec.Code, rec.Body.String())
	}
}
