package dag

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

func TestTopoLayers(t *testing.T) {
	plan := Plan{Nodes: []Node{
		{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}, {ID: "c", DependsOn: []string{"a"}},
		{ID: "d", DependsOn: []string{"b", "c"}},
	}}
	layers, err := topoLayers(plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 3 || len(layers[0]) != 1 || layers[0][0] != "a" || len(layers[1]) != 2 || len(layers[2]) != 1 || layers[2][0] != "d" {
		t.Fatalf("bad layers: %v", layers)
	}
	if _, err := topoLayers(Plan{Nodes: []Node{{ID: "x", DependsOn: []string{"y"}}, {ID: "y", DependsOn: []string{"x"}}}}); err == nil {
		t.Error("want cycle error")
	}
	if _, err := topoLayers(Plan{Nodes: []Node{{ID: "x", DependsOn: []string{"missing"}}}}); err == nil {
		t.Error("want unknown-dep error")
	}
}

func TestRunDAG_Layers(t *testing.T) {
	stub := okStub{}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{
		{ID: "n1", AgentName: "w"}, {ID: "n2", AgentName: "w"},
		{ID: "n3", AgentName: "w", DependsOn: []string{"n1", "n2"}},
	}}
	cfg := func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }
	gateNodes := map[string]workflow.Node{}
	for _, n := range plan.Nodes {
		ag, _ := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
		wn, _ := vetting.NewWorkerNode(ag)
		gateNodes[n.ID] = newGatedNode(plan, n, wn, nil, vetting.NewJudgeFactory(stub, nil), cfg("w"), nil, nil, "")
	}
	var out map[string]string
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			var err error
			out, err = runDAG(ctx, plan, gateNodes, 2)
			return "done", err
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if len(out) != 3 || out["n1"] == "" || out["n2"] == "" || out["n3"] == "" {
		t.Fatalf("runDAG outputs incomplete: %v", out)
	}
}
