// recall_round_test.go drives a real gated dispatch (not a hand-rolled dedup) to prove the
// native recall_memory bump happens once per round, on both the judge and commitFinal paths.
package vetting

import (
	"context"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
)

type recallStubArgs struct {
	Query string `json:"query"`
}
type recallStubResult struct {
	Hits []memory.Delivered `json:"hits"`
}

// recallMemoryStubTool stands in for recall_memory (internal/tools imports vetting): same name, one
// LogRecallLedgerOnly per call, always returning the seeded id.
func recallMemoryStubTool(t *testing.T, store *memory.Store, led ledger.LedgerStore, chatID, nodeID, id, content string) tool.Tool {
	t.Helper()
	hit := memory.Delivered{ID: id, Content: content, Tier: memory.TierUnverified, Score: 0.9}
	tl, err := functiontool.New[recallStubArgs, recallStubResult](
		functiontool.Config{Name: "recall_memory", Description: "Recall shared memory."},
		func(ctx adkagent.Context, _ recallStubArgs) (recallStubResult, error) {
			store.LogRecallLedgerOnly(ctx, led, chatID, nodeID, "tool", []memory.Delivered{hit})
			return recallStubResult{Hits: []memory.Delivered{hit}}, nil
		})
	if err != nil {
		t.Fatalf("recall_memory stub tool: %v", err)
	}
	return tl
}

func countRecallResponses(req *model.LLMRequest) int {
	n := 0
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "recall_memory" {
				n++
			}
		}
	}
	return n
}

// repeatRecallWorker calls recall_memory three times before answering; as judge it submits
// judgeScore so a test can force pass or fail.
func repeatRecallWorker(judgeScore float64) fnLLM {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		switch {
		case stubHasTool(req, submitVerdictTool):
			return stubCall(submitVerdictTool, map[string]any{"score": judgeScore, "feedback": "ok"}), nil
		case countRecallResponses(req) < 3:
			return stubCall("recall_memory", map[string]any{"query": "repeat"}), nil
		default:
			return stubText("the answer"), nil
		}
	}
}

// runRecallRoundNode drives one gated node whose worker calls recall_memory three times
// for id before answering, and returns the gate's final verdict.
func runRecallRoundNode(t *testing.T, id string, cfg Config, judgeScore float64) GateResult {
	t.Helper()
	stub := repeatRecallWorker(judgeScore)
	worker, err := llmagent.New(llmagent.Config{
		Name: "recall-node", Model: stub, Description: "worker", Instruction: "answer",
		Tools: []tool.Tool{recallMemoryStubTool(t, cfg.Memory, cfg.Ledger, cfg.ChatID, cfg.NodeID, id, "run go vet before committing")},
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	var res GateResult
	node, err := newTestGatedNodeCapture("recall-node", worker, stub, NewJudgeFactory(stub, nil, nil), cfg, &res)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "do the task"}}}
	for ev, err := range r.Run(t.Context(), "u", "s-recall-node", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		_ = ev
	}
	return res
}

// seedRecallableMemory commits one memory and returns its store-assigned id.
func seedRecallableMemory(t *testing.T, ctx context.Context, store *memory.Store, scope memory.Scope) string {
	t.Helper()
	if _, err := store.Commit(ctx, scope, "author", memory.Provenance{}, []memory.Candidate{{Content: "run go vet before committing"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}
	mems, _, err := store.List(ctx, []string{"role:coding"}, 0, 10, true, "")
	if err != nil || len(mems) != 1 {
		t.Fatalf("List: %v mems=%+v", err, mems)
	}
	return mems[0].ID
}

// TestRunGatedRefine_NativeRecalledMemory_CountsOnceOnJudgePass: three recalls in a round bump
// recalls once and write three ledger entries; commitFinal adds nothing on top.
func TestRunGatedRefine_NativeRecalledMemory_CountsOnceOnJudgePass(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_native_recall_pass", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	scope := memory.Scope{Role: "coding", User: "u"}
	id := seedRecallableMemory(t, ctx, store, scope)
	lgr := ledgertest.NewMemStore()

	cfg := Config{ChatID: "chat-native-pass", NodeID: "recall-node", JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", Memory: store, Ledger: lgr}
	res := runRecallRoundNode(t, id, cfg, 0.95)
	if !res.Passed {
		t.Fatalf("expected the gate to pass with a fixed high score, got Passed=false")
	}

	mem, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if mem.Recalls != 1 {
		t.Fatalf("recalls = %d, want 1 (deduped within the round; commitFinal must not add more)", mem.Recalls)
	}
	entries, err := lgr.ReadEntries(ctx, "chat-native-pass", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.Kind == ledger.KindMemoryRecall && e.NodeID == "recall-node" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("memory.recall ledger entries = %d, want 3 (every call is its own audit entry)", n)
	}
}

// TestRunGatedRefine_JudgeLess_NativeRecalledMemory_CountsOnce: a judge-less node never reaches
// prepareJudge, so commitFinal does the bump.
func TestRunGatedRefine_JudgeLess_NativeRecalledMemory_CountsOnce(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_native_recall_judgeless", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	scope := memory.Scope{Role: "coding", User: "u"}
	id := seedRecallableMemory(t, ctx, store, scope)

	cfg := Config{ChatID: "chat-judgeless", NodeID: "recall-node", JudgeRounds: 0, Memory: store}
	res := runRecallRoundNode(t, id, cfg, 0.95)
	if res.Passed {
		t.Fatalf("a judge-less node has no verdict to pass, got Passed=true")
	}

	mem, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if mem.Recalls != 1 {
		t.Fatalf("recalls = %d, want 1 (bumped by commitFinal, the only path a judge-less node reaches)", mem.Recalls)
	}
}

// TestRunGatedRefine_JudgeFailed_NativeRecalledMemory_CountsOnce: a never-passing judge still
// delivers, and recalls land once via prepareJudge.
func TestRunGatedRefine_JudgeFailed_NativeRecalledMemory_CountsOnce(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_native_recall_failed", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	scope := memory.Scope{Role: "coding", User: "u"}
	id := seedRecallableMemory(t, ctx, store, scope)

	cfg := Config{ChatID: "chat-judgefailed", NodeID: "recall-node", JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", Memory: store}
	res := runRecallRoundNode(t, id, cfg, 0.1)
	if res.Passed {
		t.Fatalf("expected the gate to fail with a fixed low score, got Passed=true")
	}

	mem, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if mem.Recalls != 1 {
		t.Fatalf("recalls = %d, want 1 (counted once via prepareJudge; commitFinal still runs on a failed round but must not add more)", mem.Recalls)
	}
}

// TestRunGatedRefine_RepeatedRoundRecall_DoesNotTripForgettingRule: three recalls in one round
// leave recalls at 1, so `recalls >= 3 && supported == 0` never matches.
func TestRunGatedRefine_RepeatedRoundRecall_DoesNotTripForgettingRule(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "test_native_recall_sweep", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	scope := memory.Scope{Role: "coding", User: "u"}
	id := seedRecallableMemory(t, ctx, store, scope)

	cfg := Config{ChatID: "chat-sweep", NodeID: "recall-node", JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", Memory: store}
	res := runRecallRoundNode(t, id, cfg, 0.95)
	if !res.Passed {
		t.Fatalf("expected the gate to pass, got Passed=false")
	}

	report, err := store.ForgetSweep(ctx, false)
	if err != nil {
		t.Fatalf("ForgetSweep: %v", err)
	}
	if report.Rules[1].Matched != 0 {
		t.Fatalf("rule 1 (recalls >= 3) matched = %d, want 0 (recalls stayed at 1 for one round's repeats)", report.Rules[1].Matched)
	}
	mem, err := store.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if mem.Status == string(memory.StatusInvalidated) {
		t.Fatalf("memory was invalidated despite recalls=%d", mem.Recalls)
	}
}
