package orchestrator

import (
	"context"
	"encoding/json"
	"iter"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

// stopModel streams one partial chunk (so the node is running), then blocks until cancelled.
type stopModel struct{ started chan struct{} }

func (stopModel) Name() string { return "stop" }

func (m stopModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
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

// TestRetryNode_StopSettlesCancelled: stopping a retry mid-node settles the node cancelled.
func TestRetryNode_StopSettlesCancelled(t *testing.T) {
	m := stopModel{started: make(chan struct{}, 1)}
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}
	planJSON, _ := json.Marshal(plan)
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.started:
			stop()
		case <-time.After(10 * time.Second):
		}
	}()
	var cancelled bool
	for ev := range orch.RetryNode(ctx, "u", "chat", "p", nil, "n1", "") {
		if d, ok := ev.Data.(stream.NodeCancelledData); ok && d.NodeID == "n1" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Error("n1 was not settled cancelled after the stop")
	}
}

// TestRetryNode_RefusesStaleStash: a stash holding another plan (a run cut mid-execute)
// must not run that plan's task under the node's id, nor announce it as the chat's plan.
func TestRetryNode_RefusesStaleStash(t *testing.T) {
	sessions := session.InMemoryService()
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, dag.NewExecutor(sessions, nil, nil, nil, nil, nil), nil, nil, nil)
	planJSON, _ := json.Marshal(dag.Plan{ID: "old", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "old task"}}})
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "chat",
		State: map[string]any{tools.ExecPlanKey: string(planJSON)}}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for ev := range orch.RetryNode(context.Background(), "u", "chat", "new", nil, "n1", "") {
		names = append(names, ev.Name)
	}
	if len(names) != 1 || names[0] != stream.EventError {
		t.Fatalf("events = %v, want exactly one error", names)
	}
}

// TestFinalizeAnswer_StoppedTerminalIsNoAnswer: once the user stopped the terminal node,
// every delivery path's finalize yields nothing, whatever draft its outputs hold.
func TestFinalizeAnswer_StoppedTerminalIsNoAnswer(t *testing.T) {
	m := stopModel{started: make(chan struct{}, 1)}
	w, err := llmagent.New(llmagent.Config{Name: "w", Model: m, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.InMemoryService()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"w": w}, map[string]model.LLM{"w": m},
		vetting.NewJudgeFactory(m, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	orch := New(sessions, nil, func(context.Context) string { return "" }, nil, ex, nil, nil, nil)
	plan := dag.Plan{ID: "p", UserMessage: "go", Nodes: []dag.Node{{ID: "n1", AgentName: "w", Task: "t"}}}
	go func() {
		<-m.started
		ex.CancelNode("chat", "n1")
	}()
	_, _, _, _ = ex.RunPlanStep(stream.WithYield(context.Background(), func(stream.SSEEvent) {}), plan, AppName, "u", "chat", nil, map[string]bool{"n1": true})
	if got := orch.finalizeAnswer(context.Background(), plan, map[string]string{"n1": "DRAFT"}, "chat"); got != "" {
		t.Errorf("finalizeAnswer = %q, want no answer for a stopped terminal node", got)
	}
}

// TestPersistAnswerMarksDeliveredAnswer: the delivered answer is marked with its turn, so a
// reload attaches it there even when a retry appends it after later turns.
func TestPersistAnswerMarksDeliveredAnswer(t *testing.T) {
	sessions := session.InMemoryService()
	o := &Orchestrator{sessions: sessions}
	if _, err := sessions.Create(context.Background(), &session.CreateRequest{AppName: AppName, UserID: "u", SessionID: "c"}); err != nil {
		t.Fatal(err)
	}
	o.persistAnswer(stream.WithTurnID(context.Background(), "t1"), "u", "c", "THE ANSWER")
	resp, err := sessions.Get(context.Background(), &session.GetRequest{AppName: AppName, UserID: "u", SessionID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range resp.Session.Events().All() {
		if turn, _ := ev.CustomMetadata[stream.DeliveredAnswerMeta].(string); turn == "t1" {
			if at, _ := ev.CustomMetadata[stream.DeliveredAtMeta].(string); at == "" {
				t.Error("the delivered answer carries no delivery time to order retries by")
			}
			return
		}
	}
	t.Fatal("the persisted answer event is not marked delivered")
}
