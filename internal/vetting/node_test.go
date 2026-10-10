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
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// stubModel routes by request shape: a request carrying submit_verdict is the judge, else the worker.
// The judge scores low until the answer is a revision, so the refine loop converges in one revise cycle.
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
		if strings.Contains(stubAllText(req), "Verdict:") {
			yield(stubText("This is the revised answer with the reviewer's fixes applied."), nil)
			return
		}
		yield(stubText("This is the initial draft answer."), nil)
	}
}

// TestGatedWorkerNode_RefineLoopConverges runs the gated-worker node end to end on the real ADK v2
// workflow engine and asserts the worker->judge refine loop revises once, then passes.
func TestGatedWorkerNode_RefineLoopConverges(t *testing.T) {
	exp := withTestTracer(t)
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

	// Exactly one "gate.revise" span: not zero (startStageSpan never called), not two
	// (a duplicate alongside dagStream's worker.round span for the same run).
	var reviseSpans int
	for _, s := range exp.GetSpans() {
		if s.Name != "quack.gate.revise" {
			continue
		}
		reviseSpans++
		attrs := map[string]string{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		if attrs["run_id"] != "worker-r1" {
			t.Errorf("gate.revise span run_id = %q, want worker-r1", attrs["run_id"])
		}
		if attrs["round"] != "1" {
			t.Errorf("gate.revise span round = %q, want 1", attrs["round"])
		}
	}
	if reviseSpans != 1 {
		t.Errorf("quack.gate.revise spans = %d, want exactly 1", reviseSpans)
	}
}

// TestGatedRefine_AdmissionFollowsPhase: across the fail-then-pass refine loop, the judge call
// runs under its own admission spec alone, never both or neither.
func TestGatedRefine_AdmissionFollowsPhase(t *testing.T) {
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}

	var calls []string
	held := "worker" // the outer newGatedNode wrapper admits the worker spec before RunGatedRefine is ever called
	cfg := Config{
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10",
		ReleaseWorker: func() {
			if held != "worker" {
				t.Errorf("ReleaseWorker called while holding %q", held)
			}
			held = ""
			calls = append(calls, "release-worker")
		},
		AdmitJudge: func(context.Context) bool {
			if held != "" {
				t.Errorf("AdmitJudge called while holding %q", held)
			}
			held = "judge"
			calls = append(calls, "admit-judge")
			return true
		},
		ReleaseJudge: func() {
			if held != "judge" {
				t.Errorf("ReleaseJudge called while holding %q", held)
			}
			held = ""
			calls = append(calls, "release-judge")
		},
		AdmitWorker: func(context.Context) bool {
			if held != "" {
				t.Errorf("AdmitWorker called while holding %q", held)
			}
			held = "worker"
			calls = append(calls, "admit-worker")
			return true
		},
	}
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
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	want := []string{
		"release-worker", "admit-judge", "release-judge", "admit-worker", // round 1 fails; revise runs as worker
		"release-worker", "admit-judge", "release-judge", "admit-worker", // round 2: passes
	}
	if len(calls) != len(want) {
		t.Fatalf("admission calls = %v, want %v", calls, want)
	}
	for i, c := range calls {
		if c != want[i] {
			t.Errorf("admission call %d = %q, want %q (full sequence %v)", i, c, want[i], calls)
		}
	}
	if held != "worker" {
		t.Errorf("held = %q after the run, want worker", held)
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

// A deterministic failure is in the FIRST judge round's prompt, and the verdict still fails by
// weakest-link despite the judge's own criterion passing at 1.0.
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

// onePassJudge always submits one load-bearing PASSING criterion.
type onePassJudge struct{}

func (onePassJudge) Name() string { return "one-pass-judge" }

func (onePassJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(stubCall(submitVerdictTool, map[string]any{
			"score":    1.0,
			"criteria": map[string]any{"accuracy": map[string]any{"score": 1.0, "reason": "solid"}},
		}), nil)
	}
}

// judgeModelCoordsSpy is onePassJudge plus a SetLedgerCoords capture, standing
// in for cfg.JudgeModel (a *tracedModel in production).
type judgeModelCoordsSpy struct {
	onePassJudge
	stamped ledger.Coords
}

func (s *judgeModelCoordsSpy) SetLedgerCoords(c ledger.Coords) { s.stamped = c }

// cfg.JudgeModel, when it implements CoordSetter, must be stamped with the SAME coords the judge
// round's ctx carries, as runWorkerNodeTraced does for workerModel.
func TestRunGatedRefine_StampsJudgeModelWithRoundCoords(t *testing.T) {
	spy := &judgeModelCoordsSpy{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stubFixedAnswerModel{text: "the answer"},
		Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{
		JudgeRounds: 1, Threshold: 0.5, Rubric: "score 0-10",
		ChatID: "chat1", Agent: "web-researcher", Source: "github", JudgeModel: spy,
		BundleHash: "bundlehash123456",
	}
	node, err := newTestGatedNode("gate", worker, stubFixedAnswerModel{}, NewJudgeFactory(spy, nil, nil), cfg)
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
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "question"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if spy.stamped.Agent != "judge" {
		t.Errorf("JudgeModel stamped Agent = %q, want %q", spy.stamped.Agent, "judge")
	}
	if spy.stamped.ChatID != "chat1" {
		t.Errorf("JudgeModel stamped ChatID = %q, want %q", spy.stamped.ChatID, "chat1")
	}
	if spy.stamped.User != "u" || spy.stamped.Source != "github" {
		t.Errorf("JudgeModel stamped User/Source = %q/%q, want u/github", spy.stamped.User, spy.stamped.Source)
	}
	// The judge round's llm.call entries must carry the worker's bundle hash too.
	if spy.stamped.BundleHash != "bundlehash123456" {
		t.Errorf("JudgeModel stamped BundleHash = %q, want %q", spy.stamped.BundleHash, "bundlehash123456")
	}
}

// Folding a computed deterministic map into a verdict still takes the lowest criterion
// overall and never touches the judge's own criteria scores.
func TestMergeDeterministic_WeakestLinkUnchanged(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.95}, "clarity": {Score: 0.9}}, Score: 0.9}
	det := map[string]criterionScore{"mermaid_valid": {Score: 0, Reason: "deterministic: invalid mermaid diagram at line 12: parse error"}}
	got := mergeDeterministic(v, det, Config{})
	if got.Score != 0 {
		t.Fatalf("score = %v, want 0 (weakest-link on the deterministic failure)", got.Score)
	}
	if got.Criteria["accuracy"].Score != 0.95 || got.Criteria["clarity"].Score != 0.9 {
		t.Errorf("judge criteria altered by the merge: %+v", got.Criteria)
	}
}

// The fix text for a mermaid_valid failure must point at check_mermaid: models follow an
// instruction in the error they're reacting to far more reliably than an upfront prompt nudge.
func TestMergeDeterministic_MermaidFixMentionsCheckTool(t *testing.T) {
	det := map[string]criterionScore{"mermaid_valid": {Score: 0, Reason: "deterministic: invalid mermaid diagram at line 12: parse error"}}
	got := mergeDeterministic(verdict{}, det, Config{})
	if !strings.Contains(got.Criteria["mermaid_valid"].Fix, "check_mermaid") {
		t.Fatalf("mermaid_valid Fix = %q, want it to mention check_mermaid", got.Criteria["mermaid_valid"].Fix)
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

// runGatedRefineOnce drives one RunGatedRefine round through the real ADK runner (stubPassJudge,
// a fixed-answer worker) and returns the captured GateResult.
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

// A node on a repo quack can't derive checks for still PASSES the gate (an unsupported build
// system is not a change failure), and GateResult carries why.
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

// A node whose derived checks actually ran and passed carries no skip reason.
func TestRunGatedRefine_ChecksSkipReasonEmptyWhenChecksRan(t *testing.T) {
	cfg, root := scopeCfg(t, "", "go", "gofmt")
	// TMPDIR/GOTMPDIR pinned to t.TempDir(): the default zero-value caps leave
	// `go build`'s work dir on a path the jail doesn't grant.
	scratch := t.TempDir()
	cfg.WorkspaceCaps.Env = map[string]string{"TMPDIR": scratch, "GOTMPDIR": scratch}
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

// Only a deterministic criterion failed: that failure leads, then the judge's notes, labelled
// as covering only what the judge was asked to score.
func TestComposeFeedbackDeterministicOnlyLeadsAndScopesJudgeNotes(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 1.0}}, Feedback: "The implementation is excellent."}
	det := map[string]criterionScore{"delivery_complete": {Score: 0, Reason: "deterministic: no commit found in the ledger"}}
	merged := mergeDeterministic(v, det, Config{})

	_, got := composeFeedback(merged, 0.7, 1)

	detIdx := strings.Index(got, "Deterministic check failures")
	feedbackIdx := strings.Index(got, "The implementation is excellent.")
	if detIdx == -1 {
		t.Fatalf("composeFeedback = %q, want a deterministic-failures header", got)
	}
	if feedbackIdx == -1 || feedbackIdx < detIdx {
		t.Errorf("composeFeedback = %q, want the deterministic failures BEFORE the judge's notes", got)
	}
	if !strings.Contains(got, "delivery_complete: deterministic: no commit found in the ledger") {
		t.Errorf("composeFeedback = %q, want the failing criterion's reason", got)
	}
	if !strings.Contains(got, "remaining criteria") {
		t.Errorf("composeFeedback = %q, want the judge's notes labelled as scoped to the remaining criteria", got)
	}
}

// A judge-scored criterion below threshold is never printed under a "deterministic" heading:
// it is an opinion, not a code-owned fact.
func TestComposeFeedbackJudgeOnlyDoesNotLabelDeterministic(t *testing.T) {
	v := verdict{
		Criteria: map[string]criterionScore{"accuracy": {Score: 0.3, Reason: "the analysis misses the caching layer"}},
		Feedback: "needs more depth",
	}
	_, got := composeFeedback(v, 0.7, 1)
	if strings.Contains(got, "Deterministic") {
		t.Errorf("composeFeedback = %q, must not label a judge-scored criterion as deterministic", got)
	}
	if !strings.Contains(got, "accuracy: the analysis misses the caching layer") {
		t.Errorf("composeFeedback = %q, want the failing criterion's reason", got)
	}
	if !strings.Contains(got, "needs more depth") {
		t.Errorf("composeFeedback = %q, want the judge's own feedback", got)
	}
}

// With both a deterministic and a judge-scored criterion failing, each is printed under its own
// heading and every criterion appears exactly once.
func TestComposeFeedbackBothKindsEachOwnHeadingNoDuplicates(t *testing.T) {
	v := verdict{
		Criteria: map[string]criterionScore{"accuracy": {Score: 0.3, Reason: "shallow analysis"}},
		Feedback: "judge notes",
	}
	det := map[string]criterionScore{"checks_pass": {Score: 0, Reason: "deterministic: go test ./... failed"}}
	merged := mergeDeterministic(v, det, Config{})

	_, got := composeFeedback(merged, 0.7, 1)

	if !strings.Contains(got, "Deterministic check failures") {
		t.Errorf("composeFeedback = %q, want a deterministic-failures heading", got)
	}
	if !strings.Contains(got, "Other criteria the judge scored below threshold") {
		t.Errorf("composeFeedback = %q, want a separate heading for the judge-scored failure", got)
	}
	for _, want := range []string{"checks_pass: deterministic: go test ./... failed", "accuracy: shallow analysis"} {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("composeFeedback contains %q %d times, want exactly 1: %q", want, n, got)
		}
	}
}

// A passing verdict's feedback is returned unchanged.
func TestComposeFeedbackPassingUnchanged(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.95}}, Feedback: "all good"}
	if _, got := composeFeedback(v, 0.7, 1); got != "all good" {
		t.Errorf("composeFeedback = %q, want unchanged judge feedback %q", got, "all good")
	}
}

// composeFeedback only reformats text: mergeDeterministic's weakest-link score is identical
// whether the failure is deterministic-only, judge-only, both, or neither.
func TestComposeFeedbackScoreUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		v         verdict
		det       map[string]criterionScore
		wantScore float64
	}{
		{
			name:      "deterministic only",
			v:         verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 1.0}}},
			det:       map[string]criterionScore{"delivery_complete": {Score: 0}},
			wantScore: 0,
		},
		{
			name:      "judge only",
			v:         verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.3}}},
			wantScore: 0.3,
		},
		{
			name:      "both",
			v:         verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.3}}},
			det:       map[string]criterionScore{"checks_pass": {Score: 0}},
			wantScore: 0,
		},
		{
			name:      "passing",
			v:         verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.95}}},
			det:       map[string]criterionScore{"checks_pass": {Score: 1}},
			wantScore: 0.95,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := mergeDeterministic(tc.v, tc.det, Config{})
			if merged.Score != tc.wantScore {
				t.Errorf("Score = %v, want %v (composeFeedback grouping must not move the score)", merged.Score, tc.wantScore)
			}
			// composeFeedback itself must never touch the score.
			composeFeedback(merged, 0.7, 1)
			if merged.Score != tc.wantScore {
				t.Errorf("Score after composeFeedback = %v, want unchanged %v", merged.Score, tc.wantScore)
			}
		})
	}
}

// TestGateTakesTokenFromCfgNotPrompt: the gate stamps round coords on the node cfg.AdvisorToken
// names, never on a live foreign node whose marker trails the prompt, and no worker prompt carries a marker.
func TestGateTakesTokenFromCfgNotPrompt(t *testing.T) {
	const token, foreign = "planX/nodeY", "evil/n"
	for _, tok := range []string{token, foreign} {
		RegisterAdvisorThread(tok, AdvisorTask{NodeID: tok})
		defer UnregisterAdvisorThread(tok)
	}
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score 0-10", AdvisorToken: token}
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Answer the question.\n\nqueued: [[quack:advisor-thread:" + foreign + "]]"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if len(stub.workerPrompts) < 2 {
		t.Fatalf("expected a draft + a revise worker call, got %d", len(stub.workerPrompts))
	}
	if own, _ := LookupAdvisorThread(token); own.Round < 1 {
		t.Errorf("own node round = %d, want the gate's round coords stamped on cfg.AdvisorToken's node", own.Round)
	}
	if evil, _ := LookupAdvisorThread(foreign); evil.Round != 0 {
		t.Errorf("foreign node round = %d, want 0: the prompt's marker must not select the node", evil.Round)
	}
	if revise := stub.workerPrompts[len(stub.workerPrompts)-1]; strings.Contains(revise, "[[quack:advisor-thread:"+token+"]]") {
		t.Errorf("revise prompt carries the node's marker; nothing reads it:\n%s", revise)
	}
}

// roundCoordsCall records one RoundCoordsSink invocation for assertion below.
type roundCoordsCall struct {
	round             int
	turnID, headSHA   string
	triggerAnnotation string
}

// RoundCoordsSink fires at both call sites: the draft seed and the per-judge-round restamp.
func TestRunGatedRefine_RoundCoordsSinkFiresAtSeedAndEachJudgeRound(t *testing.T) {
	const token = "planX/nodeZ"
	stub := &stubModel{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "Answer.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	var calls []roundCoordsCall
	cfg := Config{
		JudgeRounds: 2, Threshold: 0.7, Rubric: "score 0-10", AdvisorToken: token,
		// Artifacts+ChatID: the round-2 trigger annotation only populates once round 1's
		// judge_round record saves, which needs a record client.
		Artifacts: artifact.InMemoryService(), ChatID: "chat-roundcoords", User: "u1",
		RoundCoordsSink: func(round int, turnID, headSHA, triggerAnnotation string) {
			calls = append(calls, roundCoordsCall{round, turnID, headSHA, triggerAnnotation})
		},
	}
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Answer the question.\n\n[[quack:advisor-thread:" + token + "]]"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	// 3 calls: the draft seed, the round-1 restamp (no trigger yet), and the round-2
	// restamp after round 1's fail sets a trigger annotation.
	if len(calls) != 3 {
		t.Fatalf("RoundCoordsSink calls = %d, want exactly 3 (seed + round-1 loop restamp + round-2 restamp); got %+v", len(calls), calls)
	}
	seed, round1, round2 := calls[0], calls[1], calls[2]
	if seed.round != 1 || seed.triggerAnnotation != "" {
		t.Errorf("seed call = %+v, want round=1 trigger=\"\"", seed)
	}
	if round1.round != 1 || round1.triggerAnnotation != "" {
		t.Errorf("round-1 restamp = %+v, want round=1 trigger=\"\"", round1)
	}
	if round2.round != 2 || round2.triggerAnnotation == "" {
		t.Errorf("round-2 restamp = %+v, want round=2 non-empty trigger", round2)
	}
	if seed.turnID == "" || seed.turnID != round1.turnID || seed.turnID != round2.turnID {
		t.Errorf("turnID mismatch across calls: seed=%q round1=%q round2=%q", seed.turnID, round1.turnID, round2.turnID)
	}
	if seed.headSHA != round1.headSHA || seed.headSHA != round2.headSHA {
		t.Errorf("headSHA mismatch across calls: seed=%q round1=%q round2=%q", seed.headSHA, round1.headSHA, round2.headSHA)
	}
}

// At JudgeRounds=1 the loop judges the draft, revises on the fail, and re-judges:
// JudgeRounds counts REVISIONS (1 revision / 2 judgments).
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

// A failure record left by an earlier invocation of the same chat+node+agent must not survive into a new
// invocation whose model returns an empty answer: that must resolve as ErrNodeEmpty, not the stale error.
func TestRunGatedRefine_EntryClearDropsStaleFailureBeforeASilentGap(t *testing.T) {
	// planNodeID (RunGatedRefine's nodeID) differs from cfg.NodeID for setup-plan implementer
	// nodes, and the entry-clear must key off cfg.NodeID, the recorder's own key.
	const chatID, planNodeID, workspaceScope, agentName = "chat-1109-entryclear", "impl-1", "quack-shared-repo", "code-implementer"
	inference.RecordCallResult(chatID, workspaceScope, agentName, errors.New("stale: previous invocation's gateway error"))
	t.Cleanup(func() { inference.ClearFailure(chatID, workspaceScope, agentName) })

	stub := stubFixedAnswerModel{text: ""} // succeeds every call, always empty - the true silent-gap shape
	worker, err := llmagent.New(llmagent.Config{
		Name: agentName, Model: stub, Description: "reader",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 0, Threshold: 0.7, ChatID: chatID, NodeID: workspaceScope, Agent: agentName}
	node, err := newTestGatedNode(planNodeID, worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
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

	// A true silent gap (ErrNodeEmpty) is the EXPECTED outcome: the model succeeds on
	// every call but never has anything to say. Any other error is a real failure.
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil && !strings.Contains(err.Error(), "produced no answer") {
			t.Fatalf("run: %v", err)
		}
	}

	if _, _, _, ok := inference.LastFailure(chatID, workspaceScope, agentName); ok {
		t.Fatalf("stale failure record still present - the entry-clear did not fire, or a real (nonexistent) failure got recorded")
	}
}

// JudgeRounds=0 means "no judge at all" (judge:false media readers rely on it): even with a
// non-nil judge factory, the draft is surfaced unjudged.
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

// The judge-less fallback must mirror the round loop's TrimSpace guard: an empty or env-scaffold-only
// answer writes no "text:<node>" revision.
func TestGatedWorkerNode_JudgeLessEmptyAnswerWritesNoTextArtifact(t *testing.T) {
	// Env-scaffold-only, not whitespace: TrimSpace(answer) is non-empty so the ErrNodeEmpty
	// recovery path never fires; only stripLeadingEnvScaffold sees it as empty.
	stub := stubFixedAnswerModel{text: "<env>preamble only, no real content</env>"}
	worker, err := llmagent.New(llmagent.Config{
		Name: "blank-worker", Model: stub, Description: "worker",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	svc := artifact.InMemoryService()
	cfg := reviewerCfgWithArtifacts(t, svc, true)
	cfg.IsReviewer = false
	cfg.Artifact = ""
	cfg.JudgeRounds = 0
	nodeName := "blank-gate"
	node, err := newTestGatedNode(nodeName, worker, stub, nil, cfg)
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "anything"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	textID, err := recordstore.IdentityFor(kindText, nil, nodeName)
	if err != nil {
		t.Fatal(err)
	}
	rc := recordClient(cfg)
	if _, _, ok, err := rc.Latest(t.Context(), textID); err != nil || ok {
		t.Fatalf("text artifact must not exist for an empty answer: ok=%v err=%v", ok, err)
	}
}

// coordsCapturingModel also implements ledger.CoordSetter, to inspect what RunGatedRefine stamped
// via SetLedgerCoords (token-metric attribution relies on it; ctx.Value doesn't survive RunNode).
type coordsCapturingModel struct {
	stubFixedAnswerModel
	coords ledger.Coords
}

func (m *coordsCapturingModel) SetLedgerCoords(c ledger.Coords) { m.coords = c }

// User comes from the ADK session (never caller-set) and Source passes through from cfg,
// both stamped on the worker model like Agent.
func TestRunGatedRefine_StampsUserAndSourceOntoWorkerModel(t *testing.T) {
	stub := &coordsCapturingModel{stubFixedAnswerModel: stubFixedAnswerModel{text: "the answer"}}
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 0, Agent: "web-researcher", Source: "github"}
	node, err := newTestGatedNode("researcher-gate", worker, stub, nil, cfg)
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
	for _, err := range r.Run(t.Context(), "the-user", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if stub.coords.User != "the-user" {
		t.Errorf("coords.User = %q, want %q (the ADK session identity)", stub.coords.User, "the-user")
	}
	if stub.coords.Source != "github" {
		t.Errorf("coords.Source = %q, want %q (passed through from cfg)", stub.coords.Source, "github")
	}
	if stub.coords.Agent != "web-researcher" {
		t.Errorf("coords.Agent = %q, want %q", stub.coords.Agent, "web-researcher")
	}
}

// TestCitationOnlyFailure covers the trigger for the targeted citation-only
// revise directive: it fires only when citation-form criteria are the only failures.
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
		{"specifics_cited only", map[string]criterionScore{
			"grounded": {Score: 0.9}, "cites_sources": {Score: 0.9}, "specifics_cited": {Score: 0.5},
		}, true},
		{"both citation criteria", map[string]criterionScore{
			"grounded": {Score: 0.9}, "cites_sources": {Score: 0.4}, "specifics_cited": {Score: 0.5},
		}, true},
		{"specifics_cited plus a contradiction", map[string]criterionScore{
			"specifics_supported": {Score: 0}, "specifics_cited": {Score: 0.5},
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

// fnLLM is a model.LLM whose every call yields f's one response.
type fnLLM func(*model.LLMRequest) (*model.LLMResponse, error)

func (fnLLM) Name() string { return "fn-llm" }

func (f fnLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(f(req)) }
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

// newTestGatedNode wraps RunGatedRefine as a dynamic node, the shape dag.newGatedNode builds.
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

// erroringJudge always fails the model call, like a judge request that 400s on context size.
var erroringJudge = fnLLM(func(*model.LLMRequest) (*model.LLMResponse, error) {
	return nil, errors.New("simulated 400: request exceeds the available context size")
})

// When every judge call errors (not a low score), the gate fails CLOSED with Passed=false,
// never surfacing the answer as vetted.
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
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stub, NewJudgeFactory(erroringJudge, nil, nil), cfg, &res)
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
	// A genuine transport/model failure keeps the "unavailable" wording.
	if !strings.Contains(res.Feedback, "unavailable") {
		t.Errorf("GateResult.Feedback = %q, want it to still say the judge was unavailable - this is a real outage", res.Feedback)
	}
}

// A judge that ran but never called submit_verdict still fails closed, and the feedback says it
// did not reach a verdict - never the "unavailable" wording.
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

// A transport/model error keeps status="unavailable"; ErrJudgeNoVerdict gets its own status. judge.go
// returns a typed sentinel so this switches on errors.Is, never the error string.
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

// quack.node and quack.worker.round make no model call, so they carry no model attribute: Langfuse
// would type them GENERATION and skew per-model latency and cost. Session identity stays.
func TestWrapperSpans_ReportNoModel(t *testing.T) {
	exp := withTestTracer(t)
	cfg := Config{ChatID: "chat-1", Agent: "code-implementer", JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10"}
	if res := runGatedRefineOnce(t, cfg, "Implemented the feature; this answer is long enough to clear the length check."); !res.Passed {
		t.Fatalf("gate should pass: %+v", res)
	}

	byName := map[string]map[string]string{}
	for _, s := range exp.GetSpans() {
		attrs := map[string]string{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		byName[s.Name] = attrs
	}

	// The attribute keys that make an OTel span a Langfuse GENERATION.
	generationKeys := []string{
		"model", otelobs.GenAIRequestModel, otelobs.GenAIResponseModel, "llm.model_name",
		otelobs.GenAIUsageInputTokens, otelobs.GenAIUsageOutputTokens,
	}
	for _, name := range []string{"quack.node", "quack.worker.round"} {
		attrs, ok := byName[name]
		if !ok {
			t.Fatalf("span %q was never recorded", name)
		}
		for _, k := range generationKeys {
			if v, ok := attrs[k]; ok {
				t.Errorf("%s carries %s=%q - that types it as a GENERATION with no token usage", name, k, v)
			}
		}
		if got := attrs[otelobs.QuackModel]; got != "stub-fixed" {
			t.Errorf("%s %s = %q, want stub-fixed - the model stays readable under quack's own namespace", name, otelobs.QuackModel, got)
		}
		if got := attrs[otelobs.GenAIConversationID]; got != "chat-1" {
			t.Errorf("%s %s = %q, want chat-1 - session grouping is unrelated to the type problem", name, otelobs.GenAIConversationID, got)
		}
	}

	// Control: the real model call still reports its model - only checked when ADK's span reaches this
	// exporter, since ADK caches its tracer at init and may be bound to another test's provider.
	if attrs, ok := byName["generate_content stub-fixed"]; ok {
		if got := attrs[otelobs.GenAIRequestModel]; got != "stub-fixed" {
			t.Errorf("generation span %s = %q, want stub-fixed", otelobs.GenAIRequestModel, got)
		}
	}
}

// ACP's start+completion updates both carry the FunctionCall part for one call_id;
// the emitter forwards it once.
func TestJudgePartEmitterDedupsToolCallByID(t *testing.T) {
	var got []stream.SSEEvent
	emit := judgePartEmitter(func(ev stream.SSEEvent) { got = append(got, ev) }, "n1", "judge-r0")

	call := &genai.Part{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "read_file", Args: map[string]any{}}}
	emit(call)
	emit(call) // completion update re-pairs the same FunctionCall part
	emit(&genai.Part{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "read_file", Response: map[string]any{}}})

	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	want := []string{stream.EventAgentToolCall, stream.EventAgentToolResult}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("events = %v, want %v (exactly one agent_tool_call)", names, want)
	}
}

// A worker's recall buckets are exactly [repo, role, user]: no per-node Legacy bucket
// (Legacy is only for pre-scope agent-name buckets).
func TestMemoryScope_NoNodeIDLegacyBucket(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoDir, err := jail.Resolve("u1", "c1", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCfg := "[remote \"origin\"]\n\turl = git@github.com:acme/games.git\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "config"), []byte(gitCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MemoryRole: "coding", Workspace: jail, WorkspaceUserID: "u1", ChatID: "c1"}

	var got memory.Scope
	probe, err := functiontool.New[probeArgs, probeResult](
		functiontool.Config{Name: "probe", Description: "probe"},
		func(ctx adkagent.Context, _ probeArgs) (probeResult, error) {
			got = MemoryScope(ctx, cfg)
			return probeResult{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	stub := &toolLoopStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-implementer", Model: stub, Description: "impl",
		Instruction: "Do the task.", Tools: []tool.Tool{probe},
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := workflow.NewAgentNode(worker, workflow.NodeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := workflowagent.New(workflowagent.Config{
		Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "probe then finish"}}}
	for _, err := range r.Run(t.Context(), "u1", "review-new-commits", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	// ctx.Session() isn't wired for a tool context here, so User is unset; the point
	// is repo+role resolve with no node-id Legacy bucket.
	want := []string{"repo:github.com/acme/games", "role:coding"}
	buckets := got.Buckets()
	if len(buckets) != len(want) || buckets[0] != want[0] || buckets[1] != want[1] {
		t.Fatalf("Buckets() = %v, want %v (no node-id legacy bucket)", buckets, want)
	}
}
