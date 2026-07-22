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
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// fabricationStub reenacts the live-e2e defect (2026-07-10): the worker reads
// one file, then ANSWERS claiming a commit it never made and quoting README
// content it never read. The judge side captures the full prompt it receives,
// so the test can assert the workspace ledger reached it - the fix under
// test. The judge passes (0.9): what's being proven is the judge now HAS the
// evidence, not any particular verdict.
type fabricationStub struct {
	mu          sync.Mutex
	judgePrompt string
	workerRuns  int
}

func (*fabricationStub) Name() string { return "fabricationStub" }

func (s *fabricationStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			s.mu.Lock()
			s.judgePrompt = stubAllText(req)
			s.mu.Unlock()
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.workerRuns++
		if s.workerRuns == 1 {
			// First worker turn: read the file (the ONE workspace operation
			// that actually happens this session).
			yield(stubCall("read_file", map[string]any{"path": "README.md"}), nil)
			return
		}
		// Second turn (the tool result is now in context) - fabricate: claim a
		// commit that never happened and quote content the README does not
		// contain.
		yield(stubText("I committed the change as abc123. The README says \"run pytest in a virtualenv\"."), nil)
	}
}

// newStubReadFileTool is a test double registered under the REAL read_file
// name, so the session events it produces are exactly what activityFromSession
// ledgers in production.
func newStubReadFileTool(t *testing.T) tool.Tool {
	t.Helper()
	type args struct {
		Path string `json:"path"`
	}
	tl, err := functiontool.New[args, map[string]any](
		functiontool.Config{Name: "read_file", Description: "stub"},
		func(_ adkagent.Context, a args) (map[string]any, error) {
			return map[string]any{"content": "REAL-README-CONTENT: only make targets here", "truncated": false, "total_lines": float64(1)}, nil
		},
	)
	if err != nil {
		t.Fatalf("stub read_file: %v", err)
	}
	return tl
}

// TestJudgeSeesWorkspaceLedger drives the REAL gate loop (RunGatedRefine on
// the ADK workflow engine) with a worker that performs one read_file and then
// fabricates a commit claim. Asserts the judge's incoming prompt carries the
// workspace ledger - the read_file entry WITH its content sample - and no
// git_commit entry, giving claims_match_activity everything it needs to fail
// the fabrication.
func TestJudgeSeesWorkspaceLedger(t *testing.T) {
	stub := &fabricationStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "coder",
		Instruction: "Do the task.",
		Tools:       []tool.Tool{newStubReadFileTool(t)},
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10"}
	node, err := newTestGatedNode("coder-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name:      "root",
		SubAgents: []adkagent.Agent{worker},
		Edges:     workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	r, err := runner.New(runner.Config{
		AppName: "test", Agent: root,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Add a CONTRIBUTING.md based on the README."}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	stub.mu.Lock()
	jp := stub.judgePrompt
	stub.mu.Unlock()
	if jp == "" {
		t.Fatal("judge never ran")
	}
	if !strings.Contains(jp, "Workspace activity") {
		t.Fatalf("judge prompt has no workspace ledger:\n%s", jp)
	}
	if !strings.Contains(jp, `read_file(path="README.md")`) {
		t.Errorf("ledger missing the read_file entry:\n%s", jp)
	}
	if !strings.Contains(jp, "REAL-README-CONTENT") {
		t.Errorf("ledger missing the read content sample (quote spot-check evidence):\n%s", jp)
	}
	if strings.Contains(jp, "git_commit(") {
		t.Errorf("ledger contains a git_commit entry that never happened:\n%s", jp)
	}
}
