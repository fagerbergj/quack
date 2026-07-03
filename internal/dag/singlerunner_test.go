package dag

import (
	"context"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// runPlanSSE runs a plan the way the orchestrator does now — RunPlanInNode inside
// one orchestration node, one runner — and returns the SSE the DagStream router
// emits plus the captured node outputs. It's the single-runner replacement for the
// old ex.Execute iterator in tests. chatID keys the per-node control registry (for
// CancelNode/SteerNode); the run session is "quack"/"s".
func runPlanSSE(t *testing.T, ex *Executor, plan Plan, chatID string) ([]stream.SSEEvent, map[string]string) {
	t.Helper()
	dsOutputs := map[string]string{} // filled by DagStream for node_done
	var planOutputs map[string]string
	var mu sync.Mutex
	var events []stream.SSEEvent
	yield := func(ev stream.SSEEvent, _ error) bool {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		return true
	}
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			o, err := ex.RunPlanInNode(ctx, plan, chatID)
			planOutputs = o // authoritative node outputs
			return "done", err
		}, workflow.NodeConfig{})
	top, err := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "quack", Agent: top, SessionService: ex.sessions, AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) { yield(ev, nil) })
	ds := ex.NewDagStream(ctx, plan, "quack", "u", "s", chatID, yield, dsOutputs)
	for ev, rerr := range r.Run(ctx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: plan.UserMessage}}}, adkagent.RunConfig{}) {
		if rerr != nil {
			t.Fatalf("run: %v", rerr)
		}
		if ev == nil {
			continue
		}
		if ds.Handle(ev) {
			continue
		}
	}
	ds.Finish()
	return events, planOutputs
}
