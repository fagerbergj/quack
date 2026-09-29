package dag

import (
	"context"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// runPlanSSE runs a plan the way the orchestrator does now - as a native
// first-class-node graph (RunPlanAsGraph) - and returns the SSE the DagStream
// emits plus the captured node outputs. chatID keys BOTH the run session and the per-node control registry (CancelNode/SteerNode), matching production.
func runPlanSSE(t *testing.T, ex *Executor, plan Plan, chatID string) ([]stream.SSEEvent, map[string]string) {
	t.Helper()
	outputs := map[string]string{}
	var mu sync.Mutex
	var events []stream.SSEEvent
	yield := func(ev stream.SSEEvent, _ error) bool {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return true
	}
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) { yield(ev, nil) })
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}
	if _, err := ex.RunPlanAsGraph(ctx, plan, "quack", "u", chatID, content, yield, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	return events, outputs
}

// sinkStub answers every worker call with fixed findings and passes every judge round.
type sinkStub struct{}

func (sinkStub) Name() string { return "sinkStub" }
func (sinkStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		yield(gText("findings"), nil)
	}
}

// TestRunPlanAsGraph_MultiSinkPlan: two independent sinks run as one graph and both outputs come
// back - ADK fails a run where two terminal nodes yield output, so the plan graph fans them in.
func TestRunPlanAsGraph_MultiSinkPlan(t *testing.T) {
	worker, err := llmagent.New(llmagent.Config{Name: "w", Model: sinkStub{}, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": worker}, map[string]model.LLM{"w": sinkStub{}},
		vetting.NewJudgeFactory(sinkStub{}, nil, nil), func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "r1", AgentName: "w", Task: "one"}, {ID: "r2", AgentName: "w", Task: "two"}}}
	_, outputs := runPlanSSE(t, ex, plan, "chat")
	if outputs["r1"] == "" || outputs["r2"] == "" {
		t.Errorf("outputs = %v, want both sinks' findings", outputs)
	}
}

// TestEnsureTerminal_SingleSinkOnly: a missed capture seeds a lone sink, never one of several - the
// fallback is the last output seen, which may be another sink's.
func TestEnsureTerminal_SingleSinkOnly(t *testing.T) {
	single := Plan{Nodes: []Node{{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}}}
	out := map[string]string{"a": "A"}
	ensureTerminal(single, out, "LAST")
	if out["b"] != "LAST" {
		t.Errorf("single sink not seeded: %v", out)
	}
	multi := Plan{Nodes: []Node{{ID: "a"}, {ID: "b"}}}
	out = map[string]string{"b": "B"}
	ensureTerminal(multi, out, "B")
	if _, seeded := out["a"]; seeded {
		t.Errorf("sink a seeded with another sink's output: %v", out)
	}
}
