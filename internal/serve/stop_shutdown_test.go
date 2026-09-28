package serve

import (
	"context"
	"encoding/json"
	"iter"
	"path/filepath"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// drainBlockModel streams one partial chunk (so the node is running), then blocks until cancelled.
type drainBlockModel struct{ started chan struct{} }

func (drainBlockModel) Name() string { return "drain-block" }

func (m drainBlockModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
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

// lateRegisterPauser hides a chat's live nodes from the first hidden sweeps, as if
// the node registered after them.
type lateRegisterPauser struct {
	*dag.Executor
	hidden int
}

func (p *lateRegisterPauser) ActiveNodes(chatID string) []string {
	if p.hidden > 0 {
		p.hidden--
		return nil
	}
	return p.Executor.ActiveNodes(chatID)
}

// stopFixture is one chat with plan p1 and a dag_node record for node n1 on a real store.
type stopFixture struct {
	st             *store.Store
	chatID, userID string
	plan           dag.Plan
}

func newStopFixture(t *testing.T) stopFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := st.RowArtifactService()
	if err != nil {
		t.Fatal(err)
	}
	st.SetArtifactService(artifacts)
	f := stopFixture{st: st, chatID: "chat-stop", plan: dag.Plan{ID: "p1", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}}
	if err := st.SetChatOrigin(ctx, f.chatID, "u1", ""); err != nil {
		t.Fatal(err)
	}
	f.userID = st.SessionUserForChat(ctx, f.chatID)
	planJSON, _ := json.Marshal(f.plan)
	if err := st.SaveDagPlan(ctx, f.chatID, f.plan.ID, "turn-1", string(planJSON)); err != nil {
		t.Fatal(err)
	}
	dag.SetAgentRoster([]dag.AgentInfo{{Name: "w"}})
	t.Cleanup(func() { dag.SetAgentRoster(nil) })
	rc := recordstore.New(st.Artifacts(), orchestrator.AppName, f.userID, f.chatID)
	if _, _, err := rc.SaveStructured(ctx, "dag_node", dag.DagNodeRecord{NodeID: "n1", Agent: "w", Status: dag.StatusQueued}, "n1", recordstore.Lineage{}); err != nil {
		t.Fatalf("seed dag_node: %v", err)
	}
	return f
}

func (f stopFixture) record(t *testing.T) dag.NodeStatus {
	t.Helper()
	raw, _, ok, err := recordstore.New(f.st.Artifacts(), orchestrator.AppName, f.userID, f.chatID).Latest(context.Background(), "dag_node:n1")
	var rec dag.DagNodeRecord
	if err != nil || !ok || json.Unmarshal(raw, &rec) != nil {
		t.Fatalf("read dag_node: ok=%v err=%v", ok, err)
	}
	return rec.Status
}

func (f stopFixture) row(t *testing.T) *store.DagNode {
	t.Helper()
	n, err := f.st.GetDagNode(context.Background(), f.plan.ID, "n1")
	if err != nil || n == nil {
		t.Fatalf("GetDagNode: %v %v", n, err)
	}
	return n
}

// TestDrainActiveRuns_LateNodeStaysResumable: a node that registered after the drain's
// sweep must not be settled cancelled by the force-cancel; boot resumes it paused/shutdown.
func TestDrainActiveRuns_LateNodeStaysResumable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hidden int
	}{{"caught by the re-sweep", 1}, {"missed by both sweeps", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopFixture(t)
			m := drainBlockModel{started: make(chan struct{}, 1)}
			w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
			if err != nil {
				t.Fatal(err)
			}
			ex := dag.NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
				vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
			ex.SetNodeStateStore(f.st)

			hub := stream.NewHub()
			runCtx, cancel := context.WithCancel(stream.WithYield(context.Background(), func(ev stream.SSEEvent) {
				runlog.PersistNodeEvent(f.st, f.chatID, f.plan.ID, ev)
			}))
			hub.RegisterRun(f.chatID, "turn-1", cancel)
			go func() {
				defer hub.UnregisterRun(f.chatID)
				_, _, _, _ = ex.RunPlanStep(runCtx, f.plan, orchestrator.AppName, f.userID, f.chatID, nil, map[string]bool{"n1": true})
			}()
			select {
			case <-m.started:
			case <-time.After(10 * time.Second):
				t.Fatal("node never started")
			}
			DrainActiveRuns(hub, &lateRegisterPauser{Executor: ex, hidden: tc.hidden}, 100*time.Millisecond)

			if n := f.row(t); n.Status == string(dag.StatusCancelled) || n.Status == string(dag.StatusFailed) {
				t.Fatalf("after drain: row %s, want it left for boot", n.Status)
			}
			rep, err := f.st.ResumePausedDagNodes(context.Background(), nil)
			if err != nil || len(rep.Start) != 1 || rep.Start[0].Reason != dag.PauseShutdown {
				t.Fatalf("boot resume = %+v err=%v, want n1 resumable with reason shutdown", rep, err)
			}
			if n := f.row(t); n.Status != string(dag.StatusPaused) || n.PauseReason != string(dag.PauseShutdown) {
				t.Errorf("row = %s/%s, want paused/shutdown", n.Status, n.PauseReason)
			}
			if got := f.record(t); got != dag.StatusPaused {
				t.Errorf("dag_node record = %s, want paused like the row", got)
			}
		})
	}
}

// TestDriveResume_NoPlanSettlesNode: a resume that can't proceed (no plan in session)
// settles the node failed on row and record, so the next boot doesn't retry it.
func TestDriveResume_NoPlanSettlesNode(t *testing.T) {
	f := newStopFixture(t)
	ctx := context.Background()
	if err := f.st.UpsertDagNode(ctx, store.DagNode{PlanID: f.plan.ID, NodeID: "n1", Status: string(dag.StatusRunning)}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetNodeStatusForChat(ctx, f.chatID, "n1", string(dag.StatusPaused), string(dag.PauseShutdown), ""); err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService() // no stashed plan
	ex := dag.NewExecutor(sessions, nil, nil, nil, nil, nil)
	orch := orchestrator.New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)

	rep, err := f.st.ResumePausedDagNodes(ctx, nil)
	if err != nil || len(rep.Start) != 1 {
		t.Fatalf("first boot = %+v err=%v, want n1 to resume", rep, err)
	}
	driveResume(ctx, f.chatID, rep.Start, orch, f.st, stream.NewHub(), runlog.NewEventLog(f.st))

	if n := f.row(t); n.Status != string(dag.StatusFailed) {
		t.Errorf("row = %s, want failed", n.Status)
	}
	if got := f.record(t); got != dag.StatusFailed {
		t.Errorf("dag_node record = %s, want failed", got)
	}
	rep, err = f.st.ResumePausedDagNodes(ctx, nil)
	if err != nil || len(rep.Start)+len(rep.AwaitingInput) != 0 {
		t.Errorf("second boot = %+v err=%v, want nothing to resume", rep, err)
	}
}
