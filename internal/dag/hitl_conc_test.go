package dag

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

type okStub struct{}

func (okStub) Name() string { return "okStub" }
func (okStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9}), nil)
			return
		}
		yield(gText("ANSWER with a source [1](http://x)"), nil)
	}
}

// TestSpike_ConcurrentRunNode: can the orchestrate node RunNode two gate nodes
// concurrently (preserving the DAG's maxActive concurrency)? -race decides.
func TestSpike_ConcurrentRunNode(t *testing.T) {
	stub := okStub{}
	mk := func() workflow.Node {
		ag, _ := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
		wn, _ := vetting.NewWorkerNode(ag)
		return wn
	}
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w"}, {ID: "n2", AgentName: "w"}}}
	cfg := func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }
	gn1 := newGatedNode(plan, plan.Nodes[0], mk(), nil, nil, vetting.NewJudgeFactory(stub, nil, nil), cfg("w"), nil, nil, "", nil, nil)
	gn2 := newGatedNode(plan, plan.Nodes[1], mk(), nil, nil, vetting.NewJudgeFactory(stub, nil, nil), cfg("w"), nil, nil, "", nil, nil)

	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			var wg sync.WaitGroup
			var o1, o2 string
			var e1, e2 error
			wg.Add(2)
			go func() { defer wg.Done(); o1, e1 = workflow.RunNode[string](ctx, gn1, plan.UserMessage) }()
			go func() { defer wg.Done(); o2, e2 = workflow.RunNode[string](ctx, gn2, plan.UserMessage) }()
			wg.Wait()
			if e1 != nil {
				return "", e1
			}
			if e2 != nil {
				return "", e2
			}
			return o1 + "||" + o2, nil
		}, workflow.NodeConfig{})
	top, _ := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	r, _ := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})

	var final string
	for ev, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev != nil {
			if s, ok := ev.Output.(string); ok && strings.Contains(s, "||") {
				final = s
			}
		}
	}
	if strings.Count(final, "ANSWER") != 2 {
		t.Errorf("concurrent RunNode did not produce both outputs: %q", final)
	} else {
		t.Logf("concurrent RunNode OK: %q", final)
	}
}
