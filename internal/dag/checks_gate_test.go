package dag

import (
	"context"
	"os"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// checksChatID is the per-chat workspace scope the nodes' checks resolve their workdir
// through, shared by the fixture dir and the RunPlanAsGraph call.
const checksChatID = "chat"

// A node whose configured check fails ends judge_passed=false / score 0 though the judge
// scores 0.9; a passing check, or no checks, leaves the node unaffected.
func TestRunPlanAsGraphFoldsChecksPass(t *testing.T) {
	stub := fixedLLM("a code change was made", nil)
	ag, err := llmagent.New(llmagent.Config{Name: "coder", Model: stub, Description: "coder", Instruction: "ROLE:coder Answer."})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	// Checks run in the node's per-chat scope; a check whose cwd is missing cannot run at
	// all, so even `true` would fail and look like a broken fold.
	root, err := jail.Resolve("u", checksChatID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	cfgFor := func(context.Context, string) vetting.Config {
		return vetting.Config{
			Threshold: 0.6, JudgeRounds: 1,
			Workspace: jail, WorkspaceUserID: "u", WorkspaceCaps: workspace.DefaultCaps(),
		}
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"coder": ag}, nil,
		vetting.NewJudgeFactory(stub, nil, nil), cfgFor, nil)
	// Serial: the root nodes share one local llmagent, which is not safe for concurrent RunNode.
	ex.SetMaxActive(1)

	// The three check nodes are independent roots; "combine" fans them in so the native
	// graph has exactly one terminal.
	plan := Plan{ID: "p", UserMessage: "go", Nodes: []Node{
		{ID: "failcheck", AgentName: "coder", Task: "do it", Checks: []string{"false"}},
		{ID: "passcheck", AgentName: "coder", Task: "do it", Checks: []string{"true"}},
		{ID: "nochecks", AgentName: "coder", Task: "do it"},
		{ID: "combine", AgentName: "coder", Task: "combine", DependsOn: []string{"failcheck", "passcheck", "nochecks"}},
	}}

	// Assert on each node's stage:judge agent_complete event directly: the gateScore
	// read-back is unreliable for a native multi-root graph.
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
	// Judge events ride the ctx sink that production wires via stream.WithYield.
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
