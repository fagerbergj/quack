package dag

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// #1033: the retry/subset path runs nodes on OUR goroutines (runDAGSubset), not
// ADK's, so ADK's scheduler recover never sees them. A ctx-yield consumer that
// panics - which safeYield now re-raises rather than swallowing - would kill the
// process here with nothing to catch it. It must surface as a node error.
func TestRetryPlanInNode_ConsumerPanicBecomesNodeError(t *testing.T) {
	stub := okStub{}
	mk := func() adkagent.Agent {
		a, _ := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
		return a
	}
	agents := map[string]adkagent.Agent{"a": mk(), "b": mk()}
	ex := NewExecutor(session.InMemoryService(), agents, nil, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{
		{ID: "a", AgentName: "a"}, {ID: "b", AgentName: "b", DependsOn: []string{"a"}},
	}}

	var retryErr error
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			_, retryErr = ex.RetryPlanInNode(ctx, plan, "chat", "b", map[string]string{"a": "A-SEED"})
			return "done", nil
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	// The stage-span emitter pulls this yield from ctx and calls it on the node's
	// own goroutine - exactly where a resumed panic would land.
	runCtx := stream.WithYield(context.Background(), func(stream.SSEEvent) { panic("consumer blew up") })
	for _, err := range r.Run(runCtx, "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if retryErr == nil {
		t.Fatal("a panicking ctx-yield consumer must surface as a node error, not vanish")
	}
	if !strings.Contains(retryErr.Error(), "panicked") {
		t.Errorf("node error should name the panic, got %v", retryErr)
	}
}
