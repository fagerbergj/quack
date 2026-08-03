package dag

import (
	"bytes"
	"context"
	"encoding/json"
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
		gateNodes[n.ID] = newGatedNode(plan, n, wn, nil, nil, vetting.NewJudgeFactory(stub, nil, nil), cfg("w"), nil, nil, "", nil, nil)
	}
	var out map[string]string
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			all := map[string]bool{}
			for _, n := range plan.Nodes {
				all[n.ID] = true
			}
			var err error
			out, err = runDAGSubset(ctx, plan, gateNodes, 2, nil, all)
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

func TestRunPlanAsGraph_Chain(t *testing.T) {
	stub := okStub{}
	ag, _ := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	exec := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{
		{ID: "n1", AgentName: "w"}, {ID: "n2", AgentName: "w", DependsOn: []string{"n1"}},
	}}
	out := map[string]string{}
	paused, err := exec.RunPlanAsGraph(context.Background(), plan, "o", "u", "s",
		&genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}},
		func(stream.SSEEvent, error) bool { return true }, out, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if paused {
		t.Fatal("nothing should pause")
	}
	if len(out) != 2 || out["n1"] == "" || out["n2"] == "" {
		t.Fatalf("RunPlanAsGraph outputs incomplete: %v", out)
	}
}

// TestRunDAG_FanInDelivery: runDAG feeds a fan-in node BOTH upstream outputs
// (dep ID → text) so the synthesizer's assembled prompt carries them - the
// single-runner replacement for the old BuildWorkflow JoinNode fan-in test.
func TestRunDAG_FanInDelivery(t *testing.T) {
	stub := stubG{}
	mk := func(name, role string) adkagent.Agent {
		a, err := llmagent.New(llmagent.Config{Name: name, Model: stub, Description: name, Instruction: role + " Answer the task."})
		if err != nil {
			t.Fatalf("agent %s: %v", name, err)
		}
		return a
	}
	agents := map[string]adkagent.Agent{
		"researcher1": mk("researcher1", "ROLE:r1"),
		"researcher2": mk("researcher2", "ROLE:r2"),
		"synthesizer": mk("synthesizer", "ROLE:synth"),
	}
	ex := NewExecutor(session.InMemoryService(), agents, nil, nil, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config {
			return vetting.Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score 0-10"}
		}, nil)
	plan := Plan{ID: "p1", UserMessage: "compare alpha and beta", Nodes: []Node{
		{ID: "r1", AgentName: "researcher1", Task: "find alpha"},
		{ID: "r2", AgentName: "researcher2", Task: "find beta"},
		{ID: "synth", AgentName: "synthesizer", Task: "combine findings", DependsOn: []string{"r1", "r2"}},
	}}
	_, outputs := runPlanSSE(t, ex, plan, "chat")
	final := outputs["synth"]
	if !strings.Contains(final, "ALPHA-FINDING") || !strings.Contains(final, "BETA-FINDING") {
		t.Fatalf("synthesizer prompt missing a fan-in input; got %q", final)
	}
}

func TestRetrySet(t *testing.T) {
	plan := Plan{Nodes: []Node{
		{ID: "a"}, {ID: "b", DependsOn: []string{"a"}}, {ID: "c", DependsOn: []string{"b"}},
		{ID: "d", DependsOn: []string{"a"}}, // sibling of b, NOT downstream of b
	}}
	got := retrySet(plan, "b")
	if !got["b"] || !got["c"] || got["a"] || got["d"] || len(got) != 2 {
		t.Fatalf("retrySet(b) = %v, want {b,c}", got)
	}
	if got := retrySet(plan, "a"); len(got) != 4 {
		t.Fatalf("retrySet(a) = %v, want all 4", got)
	}
}

// TestRetryPlanInNode_ReusesUpstream: retrying b in a→b→c re-runs b and c but
// keeps a's seeded output (a is not re-run).
func TestRetryPlanInNode_ReusesUpstream(t *testing.T) {
	stub := okStub{}
	mk := func() adkagent.Agent {
		a, _ := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
		return a
	}
	agents := map[string]adkagent.Agent{"a": mk(), "b": mk(), "c": mk()}
	ex := NewExecutor(session.InMemoryService(), agents, nil, nil, vetting.NewJudgeFactory(stub, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{
		{ID: "a", AgentName: "a"}, {ID: "b", AgentName: "b", DependsOn: []string{"a"}},
		{ID: "c", AgentName: "c", DependsOn: []string{"b"}},
	}}
	seeded := map[string]string{"a": "A-SEED-KEEP", "b": "B-OLD", "c": "C-OLD"}
	var out map[string]string
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			o, err := ex.RetryPlanInNode(ctx, plan, "chat", "b", seeded)
			out = o
			return "done", err
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if out["a"] != "A-SEED-KEEP" {
		t.Errorf("a should keep its seeded output (not re-run), got %q", out["a"])
	}
	if !strings.Contains(out["b"], "ANSWER") || out["b"] == "B-OLD" {
		t.Errorf("b should be freshly re-run, got %q", out["b"])
	}
	if !strings.Contains(out["c"], "ANSWER") || out["c"] == "C-OLD" {
		t.Errorf("c should be freshly re-run, got %q", out["c"])
	}
}

// TestPlan_AttachmentsSurviveJSON guards the single-runner media path: the execute
// tool stashes the plan as JSON in session state (ExecPlanKey) and the execute node
// unmarshals it, so media attachments (image/audio bytes) must survive that round
// trip or media nodes silently lose their input.
func TestPlan_AttachmentsSurviveJSON(t *testing.T) {
	plan := Plan{
		ID: "p", UserMessage: "describe this",
		Nodes:       []Node{{ID: "n1", AgentName: "media"}},
		Attachments: []*genai.Part{{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3, 4, 5}}}},
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var got Plan
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].InlineData == nil {
		t.Fatalf("attachment lost through JSON: %+v", got.Attachments)
	}
	if got.Attachments[0].InlineData.MIMEType != "image/png" {
		t.Errorf("mime lost: %q", got.Attachments[0].InlineData.MIMEType)
	}
	if !bytes.Equal(got.Attachments[0].InlineData.Data, []byte{1, 2, 3, 4, 5}) {
		t.Errorf("attachment bytes corrupted: %v", got.Attachments[0].InlineData.Data)
	}
}
