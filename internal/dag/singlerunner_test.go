package dag

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
)

// runPlanSSE runs a plan the way the orchestrator does now - as a native
// first-class-node graph (RunPlanAsGraph) - and returns the SSE the DagStream
// emits plus the captured node outputs. chatID keys BOTH the run session and the
// per-node control registry (CancelNode/SteerNode), matching production.
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
