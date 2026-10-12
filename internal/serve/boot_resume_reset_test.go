package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestStartResumedNodes_ResetsBeforeDispatch: a resumable chat's stale terminal event is dropped before
// any dispatch, so an early subscriber can't read the previous process's run as finished.
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
		vetting.NewJudgeFactory(stub, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	ex.SetNodeStateStore(st)
	orch := orchestrator.New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID,
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatalf("session create: %v", err)
	}

	hub := stream.NewHub()
	eventLog := runlog.NewEventLog(st)
	nodes := []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}}
	startResumedNodes(ctx, nodes, orch, st, hub, eventLog, nil, 4)

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

// TestStartResumedNodes_PlanLookupFailureEndsStream: Reopen replays the transcript before driveBootRun looks
// the plan up, so a lookup that finds nothing must still end the stream, not leave a subscriber waiting.
func TestStartResumedNodes_PlanLookupFailureEndsStream(t *testing.T) {
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	chatID := "chat-no-plan"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatal(err)
	}
	js, _ := runlog.MarshalEvent(stream.ResponseCreated("turn-1"))
	if err := st.InsertChatEvent(ctx, store.ChatEvent{ChatID: chatID, Seq: 1, Event: js}); err != nil {
		t.Fatal(err)
	}
	hub := stream.NewHub()
	eventLog := runlog.NewEventLog(st)

	startResumedNodes(ctx, []store.ResumableNode{{ChatID: chatID, PlanID: "gone", NodeID: "n1"}}, nil, st, hub, eventLog, nil, 1)

	_, live, cancel, done := hub.Subscribe(chatID)
	defer cancel()
	timeout := time.After(5 * time.Second)
	for open := !done; open; {
		select {
		case _, open = <-live:
		case <-timeout:
			t.Fatal("the reopened stream was never closed after the plan lookup failed")
		}
	}
	// A cold replay must end too, so the persisted transcript closes with done.
	evs, _ := eventLog.LoadEvents(ctx, chatID, 0)
	if n := len(evs); n == 0 || func() bool { ev, _ := runlog.UnmarshalEvent(evs[n-1].Event); return ev.Name != stream.EventDone }() {
		t.Errorf("persisted transcript = %+v, want it to end with done", evs)
	}

	// A run already holding the chat owns its stream: it must be left alone.
	hub.RegisterRun(chatID, "turn-2", func() {})
	endReopenedStream(ctx, hub, eventLog, chatID)
	if !hub.HasRegisteredRun(chatID) || hub.LastSeq(chatID) != 0 {
		t.Error("endReopenedStream published into or ended a run it did not start")
	}
}
