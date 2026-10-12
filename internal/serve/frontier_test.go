package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// cutChat seeds a run killed mid-plan: plan nodes with deps, rows for the given statuses only (a node that
// never started has no row), and the chat's run still marked in flight.
func cutChat(t *testing.T, nodes []dag.Node, rows map[string]dag.NodeStatus) (*store.Store, string) {
	t.Helper()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx, chatID := context.Background(), "chat-restart"
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatalf("SetChatOrigin: %v", err)
	}
	if err := st.SaveDagPlan(ctx, chatID, "plan-1", "turn-1", "{}"); err != nil {
		t.Fatalf("SaveDagPlan: %v", err)
	}
	if err := st.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
		t.Fatalf("MarkRunActive: %v", err)
	}
	for id, status := range rows {
		if err := st.UpsertDagNode(ctx, store.DagNode{NodeID: id, PlanID: "plan-1", Status: string(status)}); err != nil {
			t.Fatalf("UpsertDagNode %s: %v", id, err)
		}
	}
	b, _ := json.Marshal(dag.Plan{ID: "plan-1", Nodes: nodes})
	if err := st.SaveExecPlan(ctx, chatID, "plan-1", string(b)); err != nil {
		t.Fatalf("SaveExecPlan: %v", err)
	}
	return st, chatID
}

func startedIDs(rs []store.ResumableNode) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.NodeID)
	}
	slices.Sort(ids)
	return ids
}

// A run killed between nodes resumes from the next one instead of failing: n2 never started, its dependency
// is done, so boot starts it (and its retry runs n3); n3 is covered by n2 and not started on its own.
func TestBootResumesTheNextNodeOfACutRun(t *testing.T) {
	st, chatID := cutChat(t, []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}, {ID: "n3", DependsOn: []string{"n2"}}},
		map[string]dag.NodeStatus{"n1": dag.StatusDone})

	resume := reconcileNodes(context.Background(), st, nil, nil)

	if got := startedIDs(resume.Start); !slices.Equal(got, []string{"n2"}) || len(resume.Settled) != 0 {
		t.Fatalf("Start = %v, Settled = %v; want [n2] and nothing ended", got, resume.Settled)
	}
	if n2 := nodeStatus(t, st, "n2"); n2.Status != string(dag.StatusPaused) || n2.PauseReason != string(dag.PauseShutdown) {
		t.Errorf("n2 = %q/%q, want paused/shutdown so its retry continues it", n2.Status, n2.PauseReason)
	}
	if c, _ := st.GetChat(context.Background(), chatID); c.RunStatus != store.RunStatusPaused {
		t.Errorf("chat run status = %q, want paused, not failed", c.RunStatus)
	}
}

// Two independent branches cut after their shared root: both start, and a node waiting on admission (a
// queued row) is resumed too.
func TestBootResumesEveryBranchOfACutRun(t *testing.T) {
	st, _ := cutChat(t, []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}, {ID: "n3", DependsOn: []string{"n1"}}},
		map[string]dag.NodeStatus{"n1": dag.StatusDone, "n3": dag.StatusQueued})

	resume := reconcileNodes(context.Background(), st, nil, nil)

	if got := startedIDs(resume.Start); !slices.Equal(got, []string{"n2", "n3"}) {
		t.Fatalf("Start = %v, want both branches [n2 n3]", got)
	}
}

// A run killed after every node finished owed only its delivery: boot delivers the sink's answer once, ends
// the chat done and tells the extension so, instead of failing the run and asking for a re-trigger.
func TestBootFinishesARunCutAfterItsNodesWereDone(t *testing.T) {
	ctx := context.Background()
	st, chatID := cutChat(t, []dag.Node{{ID: "n1"}, {ID: "n2", DependsOn: []string{"n1"}}},
		map[string]dag.NodeStatus{"n1": dag.StatusDone})
	if err := st.UpsertDagNode(ctx, store.DagNode{NodeID: "n2", PlanID: "plan-1", Status: string(dag.StatusDone), Output: "N2-ANSWER"}); err != nil {
		t.Fatalf("UpsertDagNode n2: %v", err)
	}

	resume := reconcileNodes(ctx, st, nil, nil)
	if !slices.Equal(resume.Finish, []string{chatID}) || len(resume.Start) != 0 || len(resume.Settled) != 0 {
		t.Fatalf("Finish = %v, Start = %v, Settled = %v; want only the chat to finish", resume.Finish, resume.Start, resume.Settled)
	}

	userID := st.SessionUserForChat(ctx, chatID)
	if _, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID}); err != nil {
		t.Fatalf("session create: %v", err)
	}
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{}, nil, nil, func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
	orch := orchestrator.New(st.Sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	obs := &outcomeObserver{}
	for range 2 { // a second boot over the same turn must not deliver the answer again
		finishCutRun(ctx, chatID, orch, st, stream.NewHub(), runlog.NewEventLog(st), obs)
	}

	if len(obs.outcomes) != 2 || obs.outcomes[0].Status != extsdk.RunDone || !obs.outcomes[0].PlanRan || !strings.Contains(obs.outcomes[0].Answer, "N2-ANSWER") {
		t.Fatalf("RunEnded = %+v, want done with the plan run and n2's answer", obs.outcomes)
	}
	if c, _ := st.GetChat(ctx, chatID); c.RunStatus != store.RunStatusIdle || c.ActiveTurnID != "" {
		t.Errorf("chat = %q (active turn %q), want idle (done) with no run in flight", c.RunStatus, c.ActiveTurnID)
	}
	answers := 0
	for _, ev := range orch.PriorEvents(ctx, userID, chatID) {
		if ev.CustomMetadata[stream.DeliveredAnswerMeta] == "turn-1" {
			answers++
		}
	}
	if answers != 1 {
		t.Errorf("delivered answers for the turn = %d, want 1", answers)
	}
}

// A cut run with a failed node has no clean answer: it still settles failed.
func TestBootSettlesACutRunWithAFailedNode(t *testing.T) {
	st, _ := cutChat(t, []dag.Node{{ID: "n1"}, {ID: "n2"}},
		map[string]dag.NodeStatus{"n1": dag.StatusDone, "n2": dag.StatusFailed})

	resume := reconcileNodes(context.Background(), st, nil, nil)

	if len(resume.Finish) != 0 || len(resume.Settled) != 1 {
		t.Errorf("Finish = %v, Settled = %v; want the run settled failed", resume.Finish, resume.Settled)
	}
}

// A finish cut by another shutdown leaves the chat active and tells the extension nothing, so the next boot
// finishes it again instead of stamping it idle with no answer.
func TestBootFinishCutByShutdownStaysForTheNextBoot(t *testing.T) {
	ctx := context.Background()
	st, chatID := cutChat(t, []dag.Node{{ID: "n1"}}, map[string]dag.NodeStatus{"n1": dag.StatusDone})
	userID := st.SessionUserForChat(ctx, chatID)
	if _, err := st.Sessions.Create(ctx, &session.CreateRequest{AppName: orchestrator.AppName, UserID: userID, SessionID: chatID}); err != nil {
		t.Fatalf("session create: %v", err)
	}
	ex := dag.NewExecutor(st.Sessions, map[string]adkagent.Agent{}, nil, nil, func(context.Context, string) vetting.Config { return vetting.Config{} }, nil)
	orch := orchestrator.New(st.Sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	hub, obs := stream.NewHub(), &outcomeObserver{}
	hub.MarkInterrupted(chatID)

	finishCutRun(ctx, chatID, orch, st, hub, runlog.NewEventLog(st), obs)

	if len(obs.outcomes) != 0 {
		t.Errorf("RunEnded = %+v, want none for a run shutdown cut", obs.outcomes)
	}
	if c, _ := st.GetChat(ctx, chatID); c.ActiveTurnID == "" {
		t.Error("the chat's active turn was cleared; the next boot could not finish the run")
	}
}
