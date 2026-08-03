package dag

import (
	"context"
	"iter"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// --- unit tests: the two chain-aware helpers directly ---

func TestWorkspaceNodeID(t *testing.T) {
	setup := &Setup{Repo: "r", BaseRef: "main", WorkBranch: "w"}
	cases := []struct {
		name string
		plan Plan
		node Node
		want string
	}{
		{"no setup: implementer gets its own dir", Plan{}, Node{ID: "n1", AgentName: implementerAgent}, "n1"},
		{"no setup: reviewer gets its own dir", Plan{}, Node{ID: "n1", AgentName: reviewerAgent}, "n1"},
		{"no setup: explorer gets its own dir", Plan{}, Node{ID: "n1", AgentName: explorerAgent}, "n1"},
		{"no setup: everyone else gets their own dir", Plan{}, Node{ID: "n1", AgentName: "researcher"}, "n1"},
		{"setup, non-repo agent: own dir", Plan{Setup: setup}, Node{ID: "n1", AgentName: "researcher"}, "n1"},
		{"setup, implementer (the writer): shared", Plan{Setup: setup}, Node{ID: "n1", AgentName: implementerAgent}, workspace.SharedRepoScope},
		// Read-only qualifying nodes keep their OWN dir even under Setup - it's
		// provisioned as a linked worktree of the shared clone (worktreeParentID),
		// never the shared clone directory itself (see worktreeParentID).
		{"setup, reviewer: own dir (worktree)", Plan{Setup: setup}, Node{ID: "n1", AgentName: reviewerAgent}, "n1"},
		{"setup, explorer: own dir (worktree)", Plan{Setup: setup}, Node{ID: "n1", AgentName: explorerAgent}, "n1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := workspaceNodeID(c.plan, c.node); got != c.want {
				t.Errorf("workspaceNodeID = %q, want %q", got, c.want)
			}
		})
	}
}

// TestWorktreeParentID pins the OTHER half of the picture workspaceNodeID
// alone doesn't show: a read-only qualifying node's own dir is a git
// worktree OF the shared clone, named here - "" for a writer (it gets the
// shared clone directly) and for anything with no plan.Setup.
func TestWorktreeParentID(t *testing.T) {
	setup := &Setup{Repo: "r", BaseRef: "main", WorkBranch: "w"}
	cases := []struct {
		name string
		plan Plan
		node Node
		want string
	}{
		{"no setup: nothing", Plan{}, Node{ID: "n1", AgentName: reviewerAgent}, ""},
		{"setup, implementer: not a worktree (it's the writer)", Plan{Setup: setup}, Node{ID: "n1", AgentName: implementerAgent}, ""},
		{"setup, reviewer: worktree of the shared clone", Plan{Setup: setup}, Node{ID: "n1", AgentName: reviewerAgent}, workspace.SharedRepoScope},
		{"setup, explorer: worktree of the shared clone", Plan{Setup: setup}, Node{ID: "n1", AgentName: explorerAgent}, workspace.SharedRepoScope},
		{"setup, other agent: not a worktree", Plan{Setup: setup}, Node{ID: "n1", AgentName: "researcher"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := worktreeParentID(c.plan, c.node); got != c.want {
				t.Errorf("worktreeParentID = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNonTerminalRepoChainNode(t *testing.T) {
	setup := &Setup{Repo: "r", BaseRef: "main", WorkBranch: "w"}
	plan := Plan{Setup: setup, Nodes: []Node{
		{ID: "impl1", AgentName: implementerAgent},
		{ID: "impl2", AgentName: implementerAgent, DependsOn: []string{"impl1"}},
		{ID: "review", AgentName: reviewerAgent, DependsOn: []string{"impl2"}},
	}}
	if !nonTerminalRepoChainNode(plan, plan.Nodes[0]) {
		t.Error("impl1: want non-terminal (impl2 - also a writer - depends on it)")
	}
	if nonTerminalRepoChainNode(plan, plan.Nodes[1]) {
		t.Error("impl2: want terminal - review is read-only now (its own worktree), not a writer depending on impl2's branch state")
	}
	if nonTerminalRepoChainNode(plan, plan.Nodes[2]) {
		t.Error("review: want terminal (it's read-only, never in the writer set at all)")
	}
	other := Node{ID: "synth", AgentName: "synthesizer", DependsOn: []string{"review"}}
	if nonTerminalRepoChainNode(plan, other) {
		t.Error("synth: not a writer node, want false regardless of position")
	}
	noSetup := Plan{Nodes: plan.Nodes}
	if nonTerminalRepoChainNode(noSetup, plan.Nodes[0]) {
		t.Error("plan.Setup == nil: want false - each node delivers independently")
	}
}

// --- end-to-end: a real depends_on chain run through RunPlanAsGraph ---

// stagePRArgs/stagePRResult/chainStagePRTool give the chain's worker agent a
// real stage_pr tool so a FunctionCall dispatches to an actual FunctionResponse
// (the ledger scan that fills workerActivity.stagedDelivery reads session
// events, not stub intent).
type stagePRArgs struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
type stagePRResult struct {
	Result string `json:"result"`
}

func chainStagePRTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[stagePRArgs, stagePRResult](
		functiontool.Config{Name: "stage_pr", Description: "Stage a pull request."},
		func(_ adkagent.Context, _ stagePRArgs) (stagePRResult, error) {
			return stagePRResult{Result: "staged"}, nil
		})
	if err != nil {
		t.Fatalf("stage_pr tool: %v", err)
	}
	return tl
}

// chainStub drives every node identically: stage a PR, then finish. The judge
// always passes. Used by both nodes in the chain - since they're chained
// (never concurrent), sharing one stub/agent instance is safe (see
// checks_gate_test.go's note on why that's unsafe only for TRUE concurrency).
type chainStub struct{}

func (chainStub) Name() string { return "chainStub" }

func (chainStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": "ok"}), nil)
			return
		}
		if !gHasResponse(req, "stage_pr") {
			yield(gCall("stage_pr", map[string]any{"title": "Add widget", "body": "adds it"}), nil)
			return
		}
		yield(gText("done"), nil)
	}
}

func gHasResponse(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}
	return false
}

// newChainExecutor wires an Executor + jail for the chain e2e tests: ONE
// implementer agent (shared by every node), a Deliver that forwards to
// deliverCh, and a setup stub that counts its own calls.
func newChainExecutor(t *testing.T) (ex *Executor, jail *workspace.Jail, deliverCh chan vetting.DeliveryContext, setupCalls *int32Counter) {
	t.Helper()
	stub := chainStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: implementerAgent, Model: stub, Description: "impl",
		Instruction: "ROLE Answer.", Tools: []tool.Tool{chainStagePRTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	jail, err = workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	deliverCh = make(chan vetting.DeliveryContext, 4)
	cfgFor := func(string) vetting.Config {
		return vetting.Config{
			Threshold: 0.6, JudgeRounds: 1,
			Workspace: jail, WorkspaceUserID: "u1", WorkspaceCaps: workspace.DefaultCaps(),
			Deliver: func(_ context.Context, dc vetting.DeliveryContext) ([]vetting.DeliveryItemOutcome, error) {
				deliverCh <- dc
				return nil, nil
			},
		}
	}
	ex = NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{implementerAgent: ag}, map[string]model.LLM{implementerAgent: stub}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), cfgFor, nil)
	ex.SetMaxActive(1)
	setupCalls = &int32Counter{}
	ex.SetSetup(func(context.Context, string, string, string, Setup) error {
		setupCalls.inc()
		return nil
	})
	return ex, jail, deliverCh, setupCalls
}

func runChainPlan(t *testing.T, ex *Executor, plan Plan) {
	t.Helper()
	outputs := map[string]string{}
	start := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack", "u1", "chat1", start,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("RunPlanAsGraph: %v", err)
	}
}

// (a) A two-node implementer CHAIN shares ONE clone+branch (one setupFn call)
// and delivers ONE PR - at the terminal node only, even though BOTH nodes
// stage one.
func TestRunPlanAsGraph_ChainSharesOneCloneAndDeliversOnceAtTerminal(t *testing.T) {
	ex, jail, deliverCh, setupCalls := newChainExecutor(t)
	plan := Plan{
		ID: "p", UserMessage: "go",
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{
			{ID: "impl1", AgentName: implementerAgent, Task: "part one"},
			{ID: "impl2", AgentName: implementerAgent, Task: "part two", DependsOn: []string{"impl1"}},
		},
	}
	runChainPlan(t, ex, plan)

	if got := setupCalls.get(); got != 1 {
		t.Fatalf("setup called %d times, want exactly 1 (one shared clone for the whole chain)", got)
	}

	select {
	case dc := <-deliverCh:
		if dc.NodeID != "impl2" {
			t.Errorf("delivered NodeID = %q, want %q (the chain's terminal node)", dc.NodeID, "impl2")
		}
		wantDir, err := jail.Resolve("u1", "chat1", workspace.SetupCloneDir(workspace.SharedRepoScope))
		if err != nil {
			t.Fatal(err)
		}
		if dc.CloneDir != wantDir {
			t.Errorf("CloneDir = %q, want %q (the one shared clone)", dc.CloneDir, wantDir)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery never fired")
	}
	select {
	case dc := <-deliverCh:
		t.Fatalf("a second delivery fired: %+v - want exactly ONE, at the terminal node", dc)
	default:
	}
}

// (c) Regression: a single repo-touching node (no chain) still gets its clone
// provisioned and its delivery posted, exactly as before #310.
func TestRunPlanAsGraph_SingleRepoNodeStillDelivers(t *testing.T) {
	ex, _, deliverCh, setupCalls := newChainExecutor(t)
	plan := Plan{
		ID: "p", UserMessage: "go",
		Setup: &Setup{Repo: "https://github.com/o/r", BaseRef: "main", WorkBranch: "quack/work"},
		Nodes: []Node{
			{ID: "impl", AgentName: implementerAgent, Task: "the whole thing"},
		},
	}
	runChainPlan(t, ex, plan)

	if got := setupCalls.get(); got != 1 {
		t.Fatalf("setup called %d times, want exactly 1", got)
	}
	select {
	case dc := <-deliverCh:
		if dc.NodeID != "impl" {
			t.Errorf("delivered NodeID = %q, want %q", dc.NodeID, "impl")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery never fired")
	}
}
