package vetting

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// stubModel is a deterministic model.LLM for driving the gated-worker node offline.
// It routes by request shape (no live endpoint): a request carrying the
// submit_verdict tool is the judge; anything else is the worker. The judge scores
// low until the answer is a revision; the worker produces a revision once it sees
// reviewer feedback - so the refine loop converges in exactly one revise cycle.
type stubModel struct {
	workerCalls   int
	judgeCalls    int
	workerPrompts []string // request text captured on each worker (non-judge) call
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
		m.workerPrompts = append(m.workerPrompts, stubAllText(req))
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

// stubFixedAnswerModel is a worker stub that always returns the same text with
// no tool calls - for tests that need deterministic zero-retrieval activity.
type stubFixedAnswerModel struct{ text string }

func (m stubFixedAnswerModel) Name() string { return "stub-fixed" }

func (m stubFixedAnswerModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubText(m.text), nil)
	}
}

// judgePromptCapturingModel is a judge stub that records every prompt it
// scores and always submits a high, "flawless" verdict of its own.
type judgePromptCapturingModel struct{ prompts []string }

func (m *judgePromptCapturingModel) Name() string { return "capturing-judge" }

func (m *judgePromptCapturingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.prompts = append(m.prompts, stubAllText(req))
		yield(stubCall(submitVerdictTool, map[string]any{
			"criteria": map[string]any{"accuracy": map[string]any{"score": 1.0, "reason": "solid"}},
			"feedback": "The answer is flawless.",
		}), nil)
	}
}

// TestRunGatedRefine_DeterministicFailureReachesJudgeBeforeVerdict asserts a
// deterministic failure is present in the FIRST judge round's own prompt, and
// that the verdict still fails via weakest-link despite the judge's own
// criterion passing at 1.0.
func TestRunGatedRefine_DeterministicFailureReachesJudgeBeforeVerdict(t *testing.T) {
	judgeStub := &judgePromptCapturingModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stubFixedAnswerModel{text: "The city's downtown core is walkable."},
		Description: "researcher", Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10", RequireRetrieval: true}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stubFixedAnswerModel{}, NewJudgeFactory(judgeStub, nil, nil), cfg, &res)
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Which city are you moving to?"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if len(judgeStub.prompts) == 0 {
		t.Fatal("judge was never called")
	}
	first := judgeStub.prompts[0]
	if !strings.Contains(first, "grounded_in_retrieval") {
		t.Errorf("first judge round's prompt does not mention the already-failing deterministic criterion:\n%s", first)
	}
	if !strings.Contains(first, "Do not re-score") {
		t.Errorf("first judge round's prompt does not tell the judge the criterion is already decided:\n%s", first)
	}
	if res.Passed || res.Score != 0 {
		t.Errorf("GateResult = %+v, want Passed=false Score=0 (weakest-link on grounded_in_retrieval, despite the judge's own 1.0 criterion)", res)
	}
}

// TestMergeDeterministic_WeakestLinkUnchanged pins that folding a computed
// deterministic map into a verdict still takes the lowest criterion overall,
// and never touches the judge's own criteria scores.
func TestMergeDeterministic_WeakestLinkUnchanged(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.95}, "clarity": {Score: 0.9}}, Score: 0.9}
	det := map[string]criterionScore{"mermaid_valid": {Score: 0, Reason: "deterministic: invalid mermaid diagram at line 12: parse error"}}
	got := mergeDeterministic(v, det)
	if got.Score != 0 {
		t.Fatalf("score = %v, want 0 (weakest-link on the deterministic failure)", got.Score)
	}
	if got.Criteria["accuracy"].Score != 0.95 || got.Criteria["clarity"].Score != 0.9 {
		t.Errorf("judge criteria altered by the merge: %+v", got.Criteria)
	}
}

// stubPassJudge always submits a high score with no per-criterion detail - a
// judge stub for tests that only care whether the GATE passes, not why.
type stubPassJudge struct{}

func (stubPassJudge) Name() string { return "stub-pass-judge" }

func (stubPassJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitVerdictTool, map[string]any{"score": 0.95, "feedback": "looks solid"}), nil)
	}
}

// runGatedRefineOnce drives one RunGatedRefine round through the real ADK
// runner (stubPassJudge, a fixed-answer worker) and returns the captured
// GateResult - shared harness for the #780 checks-skip-reason tests below.
func runGatedRefineOnce(t *testing.T, cfg Config, answer string) GateResult {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stubFixedAnswerModel{text: answer},
		Description: "implementer", Instruction: "Do the task.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	var res GateResult
	node, err := newTestGatedNodeCapture("impl-gate", worker, stubFixedAnswerModel{}, NewJudgeFactory(stubPassJudge{}, nil, nil), cfg, &res)
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
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the feature."}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	return res
}

// TestRunGatedRefine_ChecksSkipReasonSurfacesOnPassingUnsupportedBuild pins
// #780 test case 1: a node on a repo quack can't derive checks for still
// PASSES the gate (an unsupported build system is not a change failure), and
// GateResult carries why - the value that reaches the delivered artifact.
func TestRunGatedRefine_ChecksSkipReasonSurfacesOnPassingUnsupportedBuild(t *testing.T) {
	cfg, root := scopeCfg(t, "", "cargo")
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.JudgeRounds = 1
	cfg.Threshold = 0.7
	cfg.Rubric = "score the answer 0-10"

	res := runGatedRefineOnce(t, cfg, "Implemented the feature; this answer is long enough to clear the length check comfortably.")

	if !res.Passed {
		t.Fatalf("gate should pass (an unsupported build system is not a change failure): %+v", res)
	}
	if res.ChecksSkipReason != skipReasonUnsupportedBuild {
		t.Errorf("ChecksSkipReason = %q, want %q", res.ChecksSkipReason, skipReasonUnsupportedBuild)
	}
}

// TestRunGatedRefine_ChecksSkipReasonEmptyWhenChecksRan pins #780 test case
// 2: a node whose derived checks actually ran and passed carries no skip
// reason - a clean run says nothing extra.
func TestRunGatedRefine_ChecksSkipReasonEmptyWhenChecksRan(t *testing.T) {
	cfg, root := scopeCfg(t, "", "go", "gofmt")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package x\n\nfunc F() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.JudgeRounds = 1
	cfg.Threshold = 0.7
	cfg.Rubric = "score the answer 0-10"

	res := runGatedRefineOnce(t, cfg, "Implemented the feature; this answer is long enough to clear the length check comfortably.")

	if !res.Passed {
		t.Fatalf("gate should pass (checks compile clean): %+v", res)
	}
	if res.ChecksSkipReason != "" {
		t.Errorf("ChecksSkipReason = %q, want empty - the checks ran, a clean run says nothing extra", res.ChecksSkipReason)
	}
}

// TestGateReattachesAdvisorMarkerOnRevise pins the workspace-scope fix: a revise
// round builds a fresh prompt from cfg.Task and would drop the advisor-thread
// marker that carries the worker's per-node clone/cwd scope - so the marker must
// be re-appended, or the revise re-clones into the bare user root.
func TestGateReattachesAdvisorMarkerOnRevise(t *testing.T) {
	const token = "planX/nodeY"
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score 0-10"}
	node, err := newTestGatedNode("gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Answer the question.\n\n" + AdvisorThreadMarker(token)}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if len(stub.workerPrompts) < 2 {
		t.Fatalf("expected a draft + a revise worker call, got %d", len(stub.workerPrompts))
	}
	revise := stub.workerPrompts[len(stub.workerPrompts)-1]
	if !strings.Contains(revise, AdvisorThreadMarker(token)) {
		t.Errorf("revise prompt dropped the advisor-thread marker - the worker's tools lose their node scope and re-clone:\n%s", revise)
	}
}

// TestGatedWorkerNode_SingleRoundRevisesOnce asserts the loop semantics at
// JudgeRounds=1: JudgeRounds counts REVISIONS, so one round judges the draft,
// revises on the fail, and re-judges - 1 revision / 2 judgments. (Previously 1
// meant "judge once, never fix", which shipped failing drafts unvetted.)
func TestGatedWorkerNode_SingleRoundRevisesOnce(t *testing.T) {
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

	// The draft failed the judge, so one round (= one revision) revises and
	// re-judges: the vetted revision is surfaced.
	if !strings.Contains(final, "revised") {
		t.Fatalf("single-round gate must revise once; got the un-revised draft: %q", final)
	}
	if stub.workerCalls != 2 {
		t.Errorf("worker calls = %d, want 2 (draft + 1 revise)", stub.workerCalls)
	}
	if stub.judgeCalls != 2 {
		t.Errorf("judge calls = %d, want 2 (judge draft, then judge the revision)", stub.judgeCalls)
	}
}

// TestGatedWorkerNode_ZeroRoundsSkipsJudge pins the 0 = "no judge at all"
// contract the media readers rely on (judge:false ⇒ JudgeRounds=0): even though
// the judge factory is non-nil, JudgeRounds=0 must never invoke it, so the draft
// is surfaced unjudged.
func TestGatedWorkerNode_ZeroRoundsSkipsJudge(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "media-reader", Model: stub, Description: "reader",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 0, Threshold: 0.7, Rubric: "score the answer 0-10"}
	node, err := newTestGatedNode("reader-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
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
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if stub.judgeCalls != 0 {
		t.Errorf("judge calls = %d, want 0 (JudgeRounds=0 must never judge)", stub.judgeCalls)
	}
	if stub.workerCalls != 1 {
		t.Errorf("worker calls = %d, want 1 (draft only, no revise)", stub.workerCalls)
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

// newTestGatedNode wraps RunGatedRefine as a first-class dynamic node - the shape
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

// newTestGatedNodeCapture is newTestGatedNode plus a *GateResult out-param, for
// tests that need to inspect the gate's verdict (not just the answer text).
func newTestGatedNodeCapture(name string, worker adkagent.Agent, workerModel model.LLM, judge JudgeFactory, cfg Config, res *GateResult) (workflow.Node, error) {
	workerNode, err := NewWorkerNode(worker)
	if err != nil {
		return nil, err
	}
	fn := func(ctx adkagent.Context, task string, emit func(*session.Event) error) (string, error) {
		if strings.TrimSpace(task) == "" {
			task = contentPlainText(ctx.UserContent())
		}
		answer, r, err := RunGatedRefine(ctx, name, workerNode, workerModel, judge, cfg, task, nil, nil, emit)
		*res = r
		return answer, err
	}
	return workflow.NewDynamicNode[string, string](name, fn, workflow.NodeConfig{}), nil
}

// erroringJudge always fails the underlying model call (standing in for a judge
// request that 400s against the model's context window) - never a scored
// verdict, never a tool call.
type erroringJudge struct{}

func (erroringJudge) Name() string { return "erroring-judge" }

func (erroringJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("simulated 400: request exceeds the available context size"))
	}
}

// TestGatedWorkerNode_JudgeErrorFailsClosed proves issue #291's critical
// correctness fix: when every judge call errors (the model call itself fails,
// not a low score), the gate must fail CLOSED - Passed=false - never surface
// the answer as vetted. Before the fix, an errored judge round could leave the
// gate's verdict looking like an unscored pass instead of an explicit fail.
func TestGatedWorkerNode_JudgeErrorFailsClosed(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10"}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stub, NewJudgeFactory(erroringJudge{}, nil, nil), cfg, &res)
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
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if res.Passed {
		t.Errorf("GateResult.Passed = true after every judge call errored - a judge failure must fail closed, never pass")
	}
	if res.Score != 0 {
		t.Errorf("GateResult.Score = %v, want 0 (no verdict was ever produced)", res.Score)
	}
	// #779 test case 1: a genuine transport/model failure keeps the existing
	// "unavailable" wording unchanged.
	if !strings.Contains(res.Feedback, "unavailable") {
		t.Errorf("GateResult.Feedback = %q, want it to still say the judge was unavailable - this is a real outage", res.Feedback)
	}
}

// TestGatedWorkerNode_JudgeNoVerdictFailsClosed is issue #779's test case 2:
// a judge that RAN (read a file, spent its turns) but never called
// submit_verdict must still fail closed like TestGatedWorkerNode_JudgeErrorFailsClosed,
// but the feedback must say the judge ran and did not reach a verdict - never
// the "unavailable" wording, which is false here.
func TestGatedWorkerNode_JudgeNoVerdictFailsClosed(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	var reads int32
	readTool := newSpyReadTool(t, "some file body", &reads)
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10", JudgeMaxIterations: 2}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stub,
		NewJudgeFactory(&stuckJudge{}, []tool.Tool{readTool}, nil), cfg, &res)
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
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if res.Passed {
		t.Errorf("GateResult.Passed = true after the judge never reached a verdict - must fail closed, never pass")
	}
	if strings.Contains(res.Feedback, "unavailable") {
		t.Errorf("GateResult.Feedback = %q, want no \"unavailable\" wording - the judge ran, it just never committed a verdict", res.Feedback)
	}
	if !strings.Contains(res.Feedback, "verdict") {
		t.Errorf("GateResult.Feedback = %q, want it to say the judge ran without reaching a verdict", res.Feedback)
	}
}

// TestJudgeFailureFeedback pins the two agent_complete Status values #779
// distinguishes: a transport/model error keeps status="unavailable" and its
// wording unchanged (test case 1); ErrJudgeNoVerdict gets its own status and
// never claims unavailability (test case 2). judge.go returns a typed
// sentinel specifically so this switches on errors.Is, never the error string.
func TestJudgeFailureFeedback(t *testing.T) {
	status, feedback := judgeFailureFeedback(errors.New("dial tcp: connection refused"))
	if status != judgeStatusUnavailable {
		t.Errorf("status = %q, want %q for a genuine transport failure", status, judgeStatusUnavailable)
	}
	if !strings.Contains(feedback, "unavailable") {
		t.Errorf("feedback = %q, want the existing unavailable wording", feedback)
	}

	status, feedback = judgeFailureFeedback(ErrJudgeNoVerdict)
	if status != judgeStatusNoVerdict || status == judgeStatusUnavailable {
		t.Errorf("status = %q, want %q and distinct from %q", status, judgeStatusNoVerdict, judgeStatusUnavailable)
	}
	if strings.Contains(feedback, "unavailable") {
		t.Errorf("feedback = %q, must not claim the judge was unavailable - it ran", feedback)
	}

	// Wrapped errors still resolve via errors.Is, not string matching.
	wrapped := fmt.Errorf("vetting: judge round: %w", ErrJudgeNoVerdict)
	if status, _ := judgeFailureFeedback(wrapped); status != judgeStatusNoVerdict {
		t.Errorf("status = %q for a wrapped ErrJudgeNoVerdict, want %q", status, judgeStatusNoVerdict)
	}
}
