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

// TestRetryNode_StopSettlesCancelledWithoutError: stopping a retry mid-node settles the
// node cancelled and emits no "context canceled" error event.
func TestRetryNode_StopSettlesCancelledWithoutError(t *testing.T) {
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
	var cancelled, errored bool
	for ev := range orch.RetryNode(ctx, "u", "chat", nil, "n1", "") {
		if d, ok := ev.Data.(stream.NodeCancelledData); ok && d.NodeID == "n1" {
			cancelled = true
		}
		if ev.Name == stream.EventError {
			errored = true
		}
	}
	if !cancelled {
		t.Error("n1 was not settled cancelled after the stop")
	}
	if errored {
		t.Error("a user stop emitted an error event")
	}
}
