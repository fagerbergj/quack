package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// stubModel is a deterministic model.LLM for driving the gated-worker node offline.
// It routes by request shape (no live endpoint): a request carrying the
// submit_verdict tool is the judge; anything else is the worker. The judge scores
// low until the answer is a revision; the worker produces a revision once it sees
// reviewer feedback — so the refine loop converges in exactly one revise cycle.
type stubModel struct {
	workerCalls int
	judgeCalls  int
}

func (m *stubModel) Name() string { return "stub" }

func (m *stubModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeCalls++
			score := 0.4
			if strings.Contains(stubAllText(req), "revised") {
				score = 0.9
			}
			yield(stubCall(submitVerdictTool, map[string]any{"score": score, "feedback": "tighten the claims"}), nil)
			return
		}
		m.workerCalls++
		if strings.Contains(stubAllText(req), "Reviewer feedback") {
			yield(stubText("This is the revised answer with the reviewer's fixes applied."), nil)
			return
		}
		yield(stubText("This is the initial draft answer."), nil)
	}
}

// TestGatedWorkerNode_RefineLoopConverges runs the native gated-worker node end to
// end on the real ADK v2 workflow engine (runner + workflowagent + Start→node) and
// asserts the worker→judge refine loop revises once, then passes.
func TestGatedWorkerNode_RefineLoopConverges(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10"}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	var final string
	for ev, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil {
			continue
		}
		if s, ok := ev.Output.(string); ok && strings.TrimSpace(s) != "" {
			final = s
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil && strings.TrimSpace(p.Text) != "" {
					final = p.Text
				}
			}
		}
	}

	if !strings.Contains(final, "revised") {
		t.Fatalf("final answer should be the vetted revision, got %q", final)
	}
	if stub.workerCalls != 2 {
		t.Errorf("worker calls = %d, want 2 (round-0 draft + 1 revise)", stub.workerCalls)
	}
	if stub.judgeCalls != 2 {
		t.Errorf("judge calls = %d, want 2 (fail then pass)", stub.judgeCalls)
	}
}

// TestGatedWorkerNode_SingleRoundNoRevise asserts the loop semantics at
// JudgeRounds=1: the judge scores the draft ONCE and, on a fail, the node
// returns the (un-revised) draft under continue-but-warn — no revise cycle
// runs. This is the per-agent economics the web-researcher/synthesizer rely on.
func TestGatedWorkerNode_SingleRoundNoRevise(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10"}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	var final string
	for ev, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev == nil {
			continue
		}
		if s, ok := ev.Output.(string); ok && strings.TrimSpace(s) != "" {
			final = s
		}
		if ev.Content != nil {
			for _, p := range ev.Content.Parts {
				if p != nil && !p.Thought && p.FunctionCall == nil && p.FunctionResponse == nil && strings.TrimSpace(p.Text) != "" {
					final = p.Text
				}
			}
		}
	}

	// The draft failed the judge, but with one round there's no revision — the
	// draft is surfaced as-is.
	if strings.Contains(final, "revised") {
		t.Fatalf("single-round gate must not revise; got a revised answer: %q", final)
	}
	if stub.workerCalls != 1 {
		t.Errorf("worker calls = %d, want 1 (draft only, no revise)", stub.workerCalls)
	}
	if stub.judgeCalls != 1 {
		t.Errorf("judge calls = %d, want 1 (judged once, no second round)", stub.judgeCalls)
	}
}

// TestCitationOnlyFailure covers the trigger for the targeted citation-only
// revise directive: it fires only when cites_sources is the SOLE failing
// criterion.
func TestCitationOnlyFailure(t *testing.T) {
	const th = 0.7
	tests := []struct {
		name string
		crit map[string]criterionScore
		want bool
	}{
		{"cites only", map[string]criterionScore{
			"grounded": {Score: 0.9}, "cites_sources": {Score: 0.2},
		}, true},
		{"cites plus another fail", map[string]criterionScore{
			"grounded": {Score: 0.3}, "cites_sources": {Score: 0.2},
		}, false},
		{"other fail only", map[string]criterionScore{
			"grounded": {Score: 0.3}, "cites_sources": {Score: 0.9},
		}, false},
		{"all pass", map[string]criterionScore{
			"grounded": {Score: 0.9}, "cites_sources": {Score: 0.9},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := citationOnlyFailure(verdict{Criteria: tt.crit}, th); got != tt.want {
				t.Errorf("citationOnlyFailure = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- stub helpers ---

func stubHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func stubAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func stubText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func stubCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// newTestGatedNode wraps RunGatedRefine as a first-class dynamic node — the shape
// dag.newGatedNode builds in production (test fixture; the old exported
// NewGatedWorkerNode constructor was removed as dead code).
func newTestGatedNode(name string, worker adkagent.Agent, workerModel model.LLM, judge JudgeFactory, cfg Config) (workflow.Node, error) {
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		return nil, err
	}
	fn := func(ctx adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
		if strings.TrimSpace(task) == "" {
			task = contentPlainText(ctx.UserContent())
		}
		answer, _, err := RunGatedRefine(ctx, name, workerNode, workerModel, judge, cfg, task, nil, nil, emit)
		return answer, err
	}
	return workflow.NewDynamicNode[string, string](name, fn, workflow.NodeConfig{}), nil
}
