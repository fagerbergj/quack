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

// fabricationStub: the worker reads one file, then claims a commit and README content it never
// saw; the judge captures its prompt so the test can check the ledger reached it.
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
		// Second turn: fabricate a commit and README content.
		yield(stubText("I committed the change as abc123. The README says \"run pytest in a virtualenv\"."), nil)
	}
}

// newStubReadFileTool uses the real read_file name, so its events ledger as in production.
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

// Through the real gate loop, the judge prompt carries the read_file ledger entry with its
// sample and no git_commit entry, so the fabricated commit claim can be failed.
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
