package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
)

// fakeMemEmbedder returns a fixed unit vector for every text, so any recall
// query matches any stored point (cosine = 1) — the round-trip is exercised
// through the SCOPE filter, not embedding similarity (mirrors internal/memory's
// own fakeEmbedder).
type fakeMemEmbedder struct{}

func (fakeMemEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// echoConsolidator replies with a single ADD op that echoes back the FIRST
// staged candidate's content verbatim — enough to prove a specific staged
// candidate reached Store.Commit, without pulling in a real consolidation model.
type echoConsolidator struct{}

func (echoConsolidator) Name() string { return "echo-consolidator" }

func (echoConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		text := stubAllText(req)
		start := strings.Index(text, "STAGED CANDIDATES:\n- ")
		content := "nothing staged"
		if start >= 0 {
			rest := text[start+len("STAGED CANDIDATES:\n- "):]
			if end := strings.IndexByte(rest, '\n'); end >= 0 {
				content = rest[:end]
			}
		}
		reply := `{"ops":[{"action":"ADD","content":"` + content + `","kind":"note"}]}`
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

// fixedScoreModel is a worker+judge stub whose judge score is fixed at build
// time — enough to drive RunGatedRefine to a deterministic pass or fail without
// a scripted revise dance.
type fixedScoreModel struct{ score float64 }

func (fixedScoreModel) Name() string { return "fixed-score" }

func (m fixedScoreModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			yield(stubCall(submitVerdictTool, map[string]any{"score": m.score, "feedback": "ok"}), nil)
			return
		}
		yield(stubText("the answer"), nil)
	}
}

// runStagedMemoryNode drives one gated node whose prompt carries token's
// advisor-thread marker, with memStage pre-loaded exactly like the ACP memory
// MCP's stage_memory handler would leave it mid-round, and returns the gate's
// final verdict.
func runStagedMemoryNode(t *testing.T, nodeID, token string, cfg Config, judgeScore float64) GateResult {
	t.Helper()
	m := fixedScoreModel{score: judgeScore}
	worker, err := llmagent.New(llmagent.Config{Name: nodeID, Model: m, Description: "worker", Instruction: "answer"})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	var res GateResult
	node, err := newTestGatedNodeCapture(nodeID, worker, m, NewJudgeFactory(m, nil, nil), cfg, &res)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "do the task " + AdvisorThreadMarker(token)}}}
	for ev, err := range r.Run(t.Context(), "u", "s-"+nodeID, task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = ev
	}
	return res
}

// TestRunGatedRefine_MCPStagedMemory_CommitsOnlyOnPass pins the ACP memory MCP
// surface's staging contract (#344): a candidate the stage_memory tool appended
// to the node's MemStage lands in commitMemoryOnPass's input, and — exactly like
// a native agent's stage_memory tool call — is only ever written to shared
// memory when the gate's judge round PASSES.
func TestRunGatedRefine_MCPStagedMemory_CommitsOnlyOnPass(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_stage", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	// The scope commitMemoryOnPass actually writes with is computed by
	// MemoryScope(ctx, cfg, nodeID) — cfg.MemoryRole + the runner's session user
	// (fixed to "u" below) — so the assertions' View must match it exactly.
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", CommitMemory: true, Memory: store, MemoryRole: "coding"}
	scope := memory.Scope{Role: "coding", User: "u"}

	t.Run("fail: nothing committed", func(t *testing.T) {
		token := "plan1/fail-node"
		stage := &MemStage{}
		RegisterAdvisorThread(token, AdvisorTask{NodeID: "fail-node", Memory: store, MemoryScope: scope, Staged: stage})
		defer UnregisterAdvisorThread(token)
		stage.Add(memory.Candidate{Content: "a fact staged on a failing round", Metadata: map[string]string{"bucket": "repo"}})

		res := runStagedMemoryNode(t, "fail-node", token, cfg, 0.1)
		if res.Passed {
			t.Fatalf("expected the gate to fail with a fixed low score, got Passed=true")
		}
		resp, err := store.View(scope, nil).SearchMemory(ctx, &adkmemory.SearchRequest{Query: "fact staged"})
		if err != nil {
			t.Fatalf("SearchMemory: %v", err)
		}
		if len(resp.Memories) != 0 {
			t.Fatalf("a failed gate round must never commit staged memory, found %d", len(resp.Memories))
		}
	})

	t.Run("pass: staged candidate committed", func(t *testing.T) {
		token := "plan1/pass-node"
		stage := &MemStage{}
		RegisterAdvisorThread(token, AdvisorTask{NodeID: "pass-node", Memory: store, MemoryScope: scope, Staged: stage})
		defer UnregisterAdvisorThread(token)
		stage.Add(memory.Candidate{Content: "always run go test before committing", Metadata: map[string]string{"bucket": "repo"}})

		res := runStagedMemoryNode(t, "pass-node", token, cfg, 0.95)
		if !res.Passed {
			t.Fatalf("expected the gate to pass with a fixed high score, got Passed=false")
		}

		deadline := time.Now().Add(2 * time.Second)
		for {
			resp, err := store.View(scope, nil).SearchMemory(ctx, &adkmemory.SearchRequest{Query: "run go test"})
			if err != nil {
				t.Fatalf("SearchMemory: %v", err)
			}
			if len(resp.Memories) == 1 {
				break // commitMemoryOnPass is fire-and-forget; poll for the async write.
			}
			if time.Now().After(deadline) {
				t.Fatalf("staged candidate never committed on a passing gate round")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
