package dag

import (
	"context"
	"iter"
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

// advisorSnoopStub is okStub plus a side channel: on the worker's own call
// (not the judge's submit_verdict round) it looks up THIS round's
// AdvisorTask via the same marker/registry seam internal/acp's resolveNode
// reads, and records whether it was ReadOnly. Proves the WIRING end to end -
// nodeGateConfig's cfg.ReadOnly reaching the registered AdvisorTask a real
// ACP round would consult - not just the config computation plan_only_test.go
// already pins.
type advisorSnoopStub struct {
	mu       sync.Mutex
	sawTask  bool
	readOnly bool
}

func (s *advisorSnoopStub) Name() string { return "advisorSnoopStub" }

func (s *advisorSnoopStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if token, ok := vetting.ParseAdvisorThread(gUserText(req)); ok {
			if at, ok := vetting.LookupAdvisorThread(token); ok {
				s.mu.Lock()
				s.sawTask, s.readOnly = true, at.ReadOnly
				s.mu.Unlock()
			}
		}
		yield(gText("ANSWER with a source [1](http://x)"), nil)
	}
}

// runSingleNode is the TestRunDAG_Layers harness, trimmed to one node - the
// shared plumbing for the two tests below.
func runSingleNode(t *testing.T, plan Plan, cfg vetting.Config, stub model.LLM) {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	wn, err := vetting.NewWorkerNode(ag)
	if err != nil {
		t.Fatal(err)
	}
	gateNodes := map[string]workflow.Node{
		plan.Nodes[0].ID: newGatedNode(plan, plan.Nodes[0], wn, nil, nil, vetting.NewJudgeFactory(stub, nil, nil), cfg, nil, nil, "", nil, nil),
	}
	orchestrate := workflow.NewDynamicNode[any, string]("orch",
		func(ctx adkagent.Context, _ any, _ func(*session.Event) error) (string, error) {
			all := map[string]bool{plan.Nodes[0].ID: true}
			_, err := runDAGSubset(ctx, plan, gateNodes, 1, nil, all)
			return "done", err
		}, workflow.NodeConfig{})
	top, err := workflowagent.New(workflowagent.Config{Name: "o", Edges: workflow.Chain(workflow.Start, orchestrate)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "o", Agent: top, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(context.Background(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
}

// TestPlanOnlyAdvisorTaskIsReadOnly is #754 test case 5: a planOnly run's
// node must be read-only in the sandbox-facing AdvisorTask (what
// internal/acp's resolveNode reads to set Caps.ReadOnly per round), not
// merely in vetting.Config - the forcing #739 does happens dynamically per
// run and must survive past nodeGateConfig into the registry a real ACP round
// consults.
func TestPlanOnlyAdvisorTaskIsReadOnly(t *testing.T) {
	plan := Plan{ID: "t-planonly", UserMessage: "x", PlanOnly: true,
		Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1")
	if !cfg.ReadOnly {
		t.Fatal("precondition failed: nodeGateConfig did not force ReadOnly for a planOnly node")
	}

	stub := &advisorSnoopStub{}
	runSingleNode(t, plan, cfg, stub)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.sawTask {
		t.Fatal("the worker round never found a registered AdvisorTask for its own node")
	}
	if !stub.readOnly {
		t.Error("AdvisorTask.ReadOnly = false for a planOnly node, want true - #754 wiring gap")
	}
}

// TestNonPlanRunAdvisorTaskIsWritable is the contrast case: an ordinary
// writable node's AdvisorTask carries ReadOnly=false, so a non-read-only
// agent doesn't get its own working directory mounted RO.
func TestNonPlanRunAdvisorTaskIsWritable(t *testing.T) {
	plan := Plan{ID: "t-writable", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: implementerAgent}}}
	cfgFor := func(string) vetting.Config { return writableGateCfg() }
	cfg := nodeGateConfig(plan, plan.Nodes[0], nil, cfgFor, "chat1")
	if cfg.ReadOnly {
		t.Fatal("precondition failed: a non-planOnly node came out ReadOnly")
	}

	stub := &advisorSnoopStub{}
	runSingleNode(t, plan, cfg, stub)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.sawTask {
		t.Fatal("the worker round never found a registered AdvisorTask for its own node")
	}
	if stub.readOnly {
		t.Error("AdvisorTask.ReadOnly = true for an ordinary writable node, want false")
	}
}
