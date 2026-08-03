package dag

import (
	"context"
	"iter"
	"os"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// checksChatID is the chat id this test runs its plan under - and therefore the
// per-chat workspace scope the nodes' deterministic checks resolve their workdir
// through (<root>/<user>/<checksChatID>/…). One constant keeps the fixture dir
// and the RunPlanAsGraph argument from drifting apart.
const checksChatID = "chat"

// checksJudgeStub always votes the judge's OWN criteria a clean pass
// (score 0.9) - any fail this test observes must come from the
// deterministic checks_pass fold (§4), not from the judge itself.
type checksJudgeStub struct{}

func (checksJudgeStub) Name() string { return "checksJudgeStub" }

func (checksJudgeStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": "looks fine"}), nil)
			return
		}
		yield(gText("a code change was made"), nil)
	}
}

// TestRunPlanAsGraphFoldsChecksPass proves the full wiring end to end
// (buildGateNodes → per-node vetting.Config.Checks/Workdir → RunGatedRefine
// → foldDeterministic → checksPassCriterion): a node whose configured check
// FAILS ends up judge_passed=false / judge_final_score=0 even though the
// judge's own criteria always score 0.9; a node whose check PASSES is
// unaffected. A node with no Checks at all is untouched by either.
func TestRunPlanAsGraphFoldsChecksPass(t *testing.T) {
	stub := checksJudgeStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "coder", Model: stub, Description: "coder", Instruction: "ROLE:coder Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	// The checks run in the node's PER-CHAT scope (<root>/u/<checksChatID>/,
	// stamped onto vetting.Config.ChatID by buildGateNodes) - the same tree the
	// node's own fs/git tools write to. Create THAT dir, not the per-user root:
	// a check whose cwd does not exist cannot run at all, so even `true` would
	// fail and passcheck would look like a broken fold.
	root, err := jail.Resolve("u", checksChatID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	cfgFor := func(string) vetting.Config {
		return vetting.Config{
			Threshold: 0.6, JudgeRounds: 1,
			Workspace: jail, WorkspaceUserID: "u", WorkspaceCaps: workspace.DefaultCaps(),
		}
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"coder": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil, nil), cfgFor, nil)
	// One node at a time: the three root nodes share this ONE local llmagent, and a
	// local llmagent is not safe for concurrent RunNode (production serves agents over
	// A2A - separate sessions per call - so this only bites the test's local agent).
	// The checks-folding assertions don't depend on concurrency; serial is deterministic.
	ex.SetMaxActive(1)

	// Single-terminal native graph rule (nativegraph.go): the three
	// independent-checks nodes are roots (no DependsOn between each other -
	// the realistic shape, since a code-implementer node's checks are its
	// own); "combine" is a plain downstream fan-in so the plan has exactly
	// one terminal.
	plan := Plan{ID: "p", UserMessage: "go", Nodes: []Node{
		{ID: "failcheck", AgentName: "coder", Task: "do it", Checks: []string{"false"}},
		{ID: "passcheck", AgentName: "coder", Task: "do it", Checks: []string{"true"}},
		{ID: "nochecks", AgentName: "coder", Task: "do it"},
		{ID: "combine", AgentName: "coder", Task: "combine", DependsOn: []string{"failcheck", "passcheck", "nochecks"}},
	}}

	// Capture each node's stage:judge agent_complete event directly - it
	// carries Score/Passed/Feedback in the SSE payload itself (node.go's
	// emitJudge), independent of the session-state gateScore read-back
	// (e.Executor.gateScore/node_done) that the live SSE stream ALSO feeds;
	// asserting on the direct event avoids that separate (and, for a native
	// multi-root-node graph, currently unreliable - a pre-existing gap
	// unrelated to this change) read-back path entirely.
	judgeDone := map[string]stream.AgentCompleteData{}
	var judgeMu sync.Mutex
	record := func(ev stream.SSEEvent, _ error) bool {
		if d, ok := ev.Data.(stream.AgentCompleteData); ok && d.Stage == stream.StageJudge {
			judgeMu.Lock()
			judgeDone[d.NodeID] = d
			judgeMu.Unlock()
		}
		return true
	}
	// The judge's stage:judge events ride a sink injected on ctx (SSE-only -
	// see node.go's RunGatedRefine doc), wired in production by
	// orchestrator.go's stream.WithYield; replicate that here so this direct
	// RunPlanAsGraph call actually surfaces them.
	ctx := stream.WithYield(context.Background(), func(ev stream.SSEEvent) { record(ev, nil) })
	outputs := map[string]string{}
	start := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "go"}}}
	if _, err := ex.RunPlanAsGraph(ctx, plan, "quack", "u", checksChatID, start, record, outputs, nil); err != nil {
		t.Fatalf("RunPlanAsGraph: %v", err)
	}

	failed, ok := judgeDone["failcheck"]
	if !ok {
		t.Fatal("failcheck: no stage:judge agent_complete event")
	}
	if failed.Passed {
		t.Errorf("failcheck: Passed = true, want false (its check failed)")
	}
	if failed.Score != 0 {
		t.Errorf("failcheck: Score = %v, want 0 (weakest-link on checks_pass)", failed.Score)
	}

	passed, ok := judgeDone["passcheck"]
	if !ok {
		t.Fatal("passcheck: no stage:judge agent_complete event")
	}
	if !passed.Passed {
		t.Error("passcheck: Passed = false, want true (its check passed)")
	}

	none, ok := judgeDone["nochecks"]
	if !ok {
		t.Fatal("nochecks: no stage:judge agent_complete event")
	}
	if !none.Passed {
		t.Error("nochecks: Passed = false, want true (no checks configured; judge score 0.9 stands)")
	}
	if none.Score != 0.9 {
		t.Errorf("nochecks: Score = %v, want 0.9 (untouched by checks_pass)", none.Score)
	}
}
