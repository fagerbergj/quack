package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// TestStartResumedNodes_ResetsBeforeDispatch pins finding 5 for boot resume:
// driveResume used to reset the durable event log from inside its own
// goroutine (spawned by boundedGoRun), so a subscriber reaching the API
// before that goroutine actually ran could read the previous process's
// stale terminal event straight off the durable table. startResumedNodes
// must reset every resumable chat synchronously before dispatching any of
// them.
func TestStartResumedNodes_ResetsBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	chatID := "chat-boot-resume-reset"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	userID := st.SessionUserForChat(ctx, chatID)

	plan := dag.Plan{ID: "plan-1", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "TASK-ONE"},
	}}
	planJSON, _ := json.Marshal(plan)
	if err := st.SaveDagPlan(ctx, chatID, plan.ID, "turn-1", string(planJSON)); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.UpsertDagNode(ctx, store.DagNode{PlanID: plan.ID, NodeID: "n1", Status: string(dag.StatusPaused), PauseReason: string(dag.PauseShutdown)}); err != nil {
		t.Fatalf("UpsertDagNode: %v", err)
	}
	if err := st.SetNodeStatusForChat(ctx, chatID, "n1", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatalf("persist pause: %v", err)
	}

	// The previous process's finished run left a stale, terminal event on
	// the chat this boot is about to resume.
	js, err := runlog.MarshalEvent(stream.Errorf(staleDispatchMarker))
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: 1, Event: js}); err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}

	stub := &resumeStubLLM{}
	sessions := session.InMemoryService()
	ag, err := llmagent.New(llmagent.Config{Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetNodeStateStore(st)
	orch := orchestrator.New(sessions, nil, "", nil, ex, nil, nil, nil)
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatalf("session create: %v", err)
	}

	hub := stream.NewHub()
	eventLog := runlog.NewEventLog(st)
	nodes := []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}}
	startResumedNodes(ctx, nodes, orch, st, hub, eventLog, 4)

	evs, err := eventLog.LoadEvents(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Event, staleDispatchMarker) {
			t.Fatalf("stale marker event was still present immediately after startResumedNodes dispatched "+
				"the resume; the reset must run synchronously before any resume goroutine is dispatched, "+
				"not from inside driveResume itself: %q", ev.Event)
		}
	}
}
