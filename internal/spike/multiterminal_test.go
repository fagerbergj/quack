package spike

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// DOCUMENTED (ADK v2.0.0): a workflow allows at most ONE terminal output node. Two
// leaf nodes that BOTH complete → ErrMultipleOutputs at run. This is the second
// constraint (with the broken JoinNode fan-in) that makes a native fan-out graph
// unable to express "N parallel researchers + HITL" — hence Path B (keep runDAG,
// where execNode is the single terminal). CANARY: if this stops erroring, multiple
// terminals became allowed — revisit .quack/node-hitl-spike.md.
func TestNativeGraph_MultipleTerminalsRejected(t *testing.T) {
	a := workflow.NewFunctionNode[any, any]("a",
		func(adkagent.Context, any) (any, error) { return "a", nil }, workflow.NodeConfig{})
	b := workflow.NewFunctionNode[any, any]("b",
		func(adkagent.Context, any) (any, error) { return "b", nil }, workflow.NodeConfig{})
	edges := workflow.NewEdgeBuilder().AddFanOut(workflow.Start, a, b).Build()
	wf, err := workflowagent.New(workflowagent.Config{Name: "mt", Edges: edges})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r, _ := runner.New(runner.Config{AppName: "mt", Agent: wf, SessionService: session.InMemoryService(), AutoCreateSession: true})
	var runErr error
	for _, e := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if e != nil {
			runErr = e
		}
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "terminal") {
		t.Fatalf("want a multiple-terminal-output error, got %v", runErr)
	}
	t.Logf("documented: two completing leaves rejected: %v", runErr)
}
