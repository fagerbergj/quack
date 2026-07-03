package spike

import (
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

// DOCUMENTED (ADK v2.0.0): a plain node cannot be a fan-in target — a node with ≥2
// unconditional incoming edges fails at build and demands a JoinNode. This is why
// Path A′ keeps fan-in out of the graph (synthesis in Go). CANARY: if this stops
// erroring, plain fan-in became supported — revisit .quack/node-hitl-spike.md.
func TestNativeGraph_PlainFanInUnsupported(t *testing.T) {
	a := workflow.NewFunctionNode[any, any]("a",
		func(adkagent.Context, any) (any, error) { return "a", nil }, workflow.NodeConfig{})
	b := workflow.NewFunctionNode[any, any]("b",
		func(adkagent.Context, any) (any, error) { return "b", nil }, workflow.NodeConfig{})
	// synth has two unconditional in-edges and is NOT a JoinNode.
	synth := workflow.NewEmittingFunctionNode[any, any]("synth",
		func(_ adkagent.Context, in any, _ func(*session.Event) error) (any, error) { return in, nil },
		workflow.NodeConfig{})

	edges := workflow.NewEdgeBuilder().
		AddFanOut(workflow.Start, a, b).
		AddFanIn(synth, a, b).
		Build()

	_, err := workflowagent.New(workflowagent.Config{Name: "pfi", Edges: edges})
	if err == nil {
		t.Fatal("expected a fan-in build error; plain multi-in-edge fan-in now works?")
	}
	if !strings.Contains(err.Error(), "JoinNode") {
		t.Fatalf("unexpected error (want mention of JoinNode): %v", err)
	}
	t.Logf("documented: plain multi-in-edge fan-in rejected at build: %v", err)
}
