// baseprompt_test.go: a queued-message re-run rebuilds its prompt from
// basePrompt - the recalled memory and preloads must survive that rebuild,
// so basePrompt is captured AFTER the prefill recall, not before (#1404 review).
package vetting

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

	"github.com/fagerbergj/quack/internal/memory"
)

// basePromptQueued is the steer marker: one message, delivered once, at the
// first gate boundary after the draft round.
const basePromptQueued = "QUEUED-EXTRA-REQUIREMENT"

const basePromptRecall = "always run go test before committing"

type basePromptCtrl struct{ taken bool }

func (c *basePromptCtrl) Cancelled() bool               { return false }
func (c *basePromptCtrl) Paused() bool                  { return false }
func (c *basePromptCtrl) PauseForInput(string)          {}
func (c *basePromptCtrl) MarkDelivered()                {}
func (c *basePromptCtrl) RepeatFailure() (string, bool) { return "", false }
func (c *basePromptCtrl) TakeQueued() string {
	if c.taken {
		return ""
	}
	c.taken = true
	return basePromptQueued
}

// basePromptStub: dual-role worker/judge stub that records every worker-round
// prompt; the worker answers in text, the judge passes on the first verdict.
type basePromptStub struct {
	mu      sync.Mutex
	prompts []string
}

func (s *basePromptStub) Name() string { return "base-prompt-stub" }

func (s *basePromptStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.95, "feedback": "ok"}), nil)
			return
		}
		s.mu.Lock()
		s.prompts = append(s.prompts, requestText(req))
		s.mu.Unlock()
		yield(stubText("the answer"), nil)
	}
}

// TestRunGatedRefine_QueuedRerunKeepsRecalledMemory: the re-run after a queued
// message folds in basePrompt + the message. If basePrompt was captured before
// the prefill recall, the re-run silently drops the recalled memory.
func TestRunGatedRefine_QueuedRerunKeepsRecalledMemory(t *testing.T) {
	ctx := t.Context()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMemEmbedder{}, echoConsolidator{}, "base_prompt", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := store.Commit(ctx, memory.Scope{Role: "coding", Legacy: "n1"}, "author",
		memory.Provenance{ChatID: "chat1"}, []memory.Candidate{{Content: basePromptRecall}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}

	stub := &basePromptStub{}
	worker, err := llmagent.New(llmagent.Config{Name: "n1", Model: stub, Description: "worker", Instruction: "answer"})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		ChatID: "chat1", Agent: "worker", NodeID: "n1", JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10",
		ExternalWorker: true, CommitMemory: true, Memory: store, MemoryRole: "coding", Task: "run tests",
	}
	ctrl := &basePromptCtrl{}
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		t.Fatalf("worker node: %v", err)
	}
	node := workflow.NewDynamicNode[string, string]("n1",
		func(c adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
			answer, _, err := RunGatedRefine(c, "n1", workerNode, stub, NewJudgeFactory(nil, stub, nil, nil), cfg, task, nil, ctrl, emit)
			return answer, err
		}, workflow.NodeConfig{})
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
	for _, err := range r.Run(ctx, "u", "s-n1", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.prompts) < 2 {
		t.Fatalf("expected a draft round and a queued re-run (2+ worker prompts), got %d", len(stub.prompts))
	}
	draft, rerun := stub.prompts[0], stub.prompts[1]
	if !strings.Contains(draft, basePromptRecall) {
		t.Fatalf("draft round lost the prefill recall; prompt was:\n%s", draft)
	}
	if strings.Contains(draft, basePromptQueued) {
		t.Fatalf("queued message appeared before it was delivered; prompt was:\n%s", draft)
	}
	if !strings.Contains(rerun, basePromptQueued) {
		t.Fatalf("re-run dropped the queued message; prompt was:\n%s", rerun)
	}
	if !strings.Contains(rerun, basePromptRecall) {
		t.Fatalf("queued re-run dropped the recalled memory (basePrompt captured before recall); prompt was:\n%s", rerun)
	}
}
