package rest

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// ctxLedger fails an append on a done ctx, as PGStore does.
type ctxLedger struct{ *ledgertest.MemStore }

func (l ctxLedger) AppendIntent(ctx context.Context, e ledger.Entry) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return l.MemStore.AppendIntent(ctx, e)
}

// midNodeModel streams one partial chunk (so the node is running), then blocks until cancelled.
type midNodeModel struct{ started chan struct{} }

func (midNodeModel) Name() string { return "mid-node" }

func (m midNodeModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if !yield(&model.LLMResponse{Content: genai.NewContentFromText("working", genai.RoleModel), Partial: true}, nil) {
			return
		}
		select {
		case m.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// TestStoppedRunSettlesNodeAndChat: stopping a run mid-node must leave the node
// row and dag_node record terminal, the chat not "running", and node.cancelled in the ledger.
func TestStoppedRunSettlesNodeAndChat(t *testing.T) {
	m := midNodeModel{started: make(chan struct{}, 1)}
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t)
	ex := dag.NewExecutor(h.store.Sessions, map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	led := ctxLedger{ledgertest.NewMemStore()}
	ex.SetWALLedger(led)
	dag.SetAgentRoster([]dag.AgentInfo{{Name: "w"}})
	t.Cleanup(func() { dag.SetAgentRoster(nil) })

	bg := context.Background()
	chatID := mustCreateChat(t, h)
	const planID, nodeID = "p1", "w-1"
	seedPlan(t, h, chatID, planID, nodeID)
	userID := h.store.SessionUserForChat(bg, chatID)
	rc := recordstore.New(h.store.Artifacts(), artifactref.AppName, userID, chatID)
	if _, _, err := rc.SaveStructured(bg, "dag_node", dag.DagNodeRecord{NodeID: nodeID, Agent: "w", Status: dag.StatusQueued}, nodeID, recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node: %v", err)
	}

	runCtx, stop := context.WithCancel(bg)
	runCtx = stream.WithYield(runCtx, func(ev stream.SSEEvent) { runlog.PersistNodeEvent(h.store, chatID, planID, ev) })
	plan := dag.Plan{ID: planID, UserMessage: "go", Nodes: []dag.Node{{ID: nodeID, AgentName: "w", Task: "t"}}}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := ex.RunPlanStep(runCtx, plan, artifactref.AppName, userID, chatID, nil, map[string]bool{nodeID: true})
		done <- err
	}()
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("node never started")
	}
	if row, _ := h.store.GetDagNode(bg, planID, nodeID); row == nil || row.Status != string(dag.StatusRunning) {
		t.Fatalf("before stop: node row = %+v, want running", row)
	}
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunPlanStep err = nil, want the stop's error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not end after stop")
	}

	if row, _ := h.store.GetDagNode(bg, planID, nodeID); row == nil || row.Status != string(dag.StatusCancelled) {
		t.Errorf("node row = %+v, want cancelled", row)
	}
	raw, _, ok, err := rc.Latest(bg, "dag_node:"+nodeID)
	var rec dag.DagNodeRecord
	if err != nil || !ok || json.Unmarshal(raw, &rec) != nil || rec.Status != dag.StatusCancelled {
		t.Errorf("dag_node record = %+v (ok=%v err=%v), want cancelled", rec, ok, err)
	}
	rr := httptest.NewRecorder()
	h.GetChat(rr, httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID, nil), chatID)
	var detail schema.ChatDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("GetChat: %d %s", rr.Code, rr.Body.String())
	}
	if detail.Status != schema.ChatStatusIdle {
		t.Errorf("chat status = %q, want idle", detail.Status)
	}
	entries, _ := led.ReadEntries(bg, chatID, 0)
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != ledger.KindNodeCancelled {
		t.Errorf("ledger kinds = %v, want a trailing %s", kinds, ledger.KindNodeCancelled)
	}
}

// TestStoppedRunEmitsNoError: a user stop is not an error, so no "context canceled"
// error event reaches the chat's durable stream.
func TestStoppedRunEmitsNoError(t *testing.T) {
	m := newGatedModel(0)
	h := newTestHandlerWithModel(t, m)
	chatID := mustCreateChat(t, h)
	srv := runServer(t, h, chatID)
	resp, err := http.Post(srv.URL+"/api/v1/chats/c1/responses", "application/json", strings.NewReader(`{"content":"hello"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	responseID := readResponseID(t, resp)
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("model was never called")
	}
	if !h.hub.CancelResponse(chatID, responseID) {
		t.Fatal("cancel found no run")
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.hub.HasRegisteredRun(chatID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := h.store.LoadChatEvents(context.Background(), chatID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if ev, _ := runlog.UnmarshalEvent(r.Event); ev.Name == stream.EventError {
			t.Errorf("stopped run persisted an error event: %s", r.Event)
		}
	}
}

// TestStoppedStartNodeEmitsNoError: the same for a node started via StartNode, whose
// orchestrator path yields its own "resume: ... context canceled" error event.
func TestStoppedStartNodeEmitsNoError(t *testing.T) {
	m := midNodeModel{started: make(chan struct{}, 1)}
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(t)
	ex := dag.NewExecutor(h.store.Sessions, map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	h.orch = orchestrator.New(h.store.Sessions, stubModel{}, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	bg := context.Background()
	chatID := mustCreateChat(t, h)
	seedPlan(t, h, chatID, "p1", "n1")
	plan := dag.Plan{ID: "p1", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}
	planJSON, _ := json.Marshal(plan)
	if _, err := h.store.Sessions.Create(bg, &session.CreateRequest{AppName: orchestrator.AppName, UserID: h.store.SessionUserForChat(bg, chatID),
		SessionID: chatID, State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}
	dp, err := h.store.GetLatestDagPlan(bg, chatID)
	if err != nil || dp == nil {
		t.Fatalf("GetLatestDagPlan: %v", err)
	}
	if !h.startNodeAsync(dp, chatID, "n1", "go") {
		t.Fatal("start not dispatched")
	}
	select {
	case <-m.started:
	case <-time.After(10 * time.Second):
		t.Fatal("node never started")
	}
	h.hub.CancelResponse(chatID, dp.TurnID)
	deadline := time.Now().Add(10 * time.Second)
	for h.hub.HasRegisteredRun(chatID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := h.store.LoadChatEvents(bg, chatID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if ev, _ := runlog.UnmarshalEvent(r.Event); ev.Name == stream.EventError {
			t.Errorf("stopped StartNode persisted an error event: %s", r.Event)
		}
	}
}
