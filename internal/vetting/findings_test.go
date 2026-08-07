package vetting

import (
	"context"
	"iter"
	"slices"
	"strings"
	"sync/atomic"
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

// scriptedFindingsJudge submits a fixed verdict on the first turn - criteria,
// findings, or both, however the test wants to script the judge's own
// per-finding call. Mirrors oneShotJudge/recordingJudge in judge_test.go.
type scriptedFindingsJudge struct {
	score    float64
	criteria map[string]any
	findings []map[string]any
}

func (scriptedFindingsJudge) Name() string { return "scripted-findings-judge" }

func (j scriptedFindingsJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		args := map[string]any{"score": j.score, "feedback": ""}
		if j.criteria != nil {
			args["criteria"] = j.criteria
		}
		if j.findings != nil {
			args["findings"] = j.findings
		}
		yield(stubCall(submitVerdictTool, args), nil)
	}
}

// TestJudgeFindings_ContradictedSinksGroundingCriterion pins the #494
// regression this PR fixes: a judge that scores claims_grounded high on its
// OWN holistic read (exactly what shipped a false "off-by-one at
// mermaid.go:112" finding with claims_grounded=1) must still have the
// criterion forced to 0 once its OWN per-finding verification contradicts a
// staged finding - proving the code-owned fold, not the judge's guess, is
// what the gate trusts. Against the pre-#498 code (no Findings field, no
// applyFindingsVerdict fold) this fails: claims_grounded stays at the judge's
// self-reported 0.9 and the verdict passes.
func TestJudgeFindings_ContradictedSinksGroundingCriterion(t *testing.T) {
	judge := scriptedFindingsJudge{
		score:    0.9,
		criteria: map[string]any{"claims_grounded": map[string]any{"score": 9, "reason": "looks precise and specific"}},
		findings: []map[string]any{
			{"index": 1, "path": "internal/vetting/mermaid.go", "line": 112, "status": "contradicted",
				"why": "line 112 is a blank line inside a doc comment, not a loop bound - there is no off-by-one here"},
		},
	}
	factory := NewJudgeFactory(judge, nil, nil)
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Comments: []ReviewComment{
			{Path: "internal/vetting/mermaid.go", Line: 112, Body: "off-by-one: the loop should stop at len(nodes)-1"},
		}},
	}}
	cfg := Config{Rubric: "score 0-10", IsReviewer: true, Threshold: 0.7}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review this PR"}}}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "LGTM.", act, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	cs, ok := v.Criteria[findingsGroundingCriterion]
	if !ok || cs.Score != 0 {
		t.Fatalf("%s = %+v, want sunk to 0 by the contradicted finding", findingsGroundingCriterion, cs)
	}
	if v.Score >= cfg.Threshold {
		t.Fatalf("verdict score = %v, want below threshold %v (weakest-link over %s)", v.Score, cfg.Threshold, findingsGroundingCriterion)
	}
}

// TestJudgeFindings_VerifiedFindingNoPenalty proves the mirror case: a
// finding the judge verifies against the code must NOT move
// findingsGroundingCriterion away from whatever the judge itself scored -
// verification is informational, not an automatic bonus or malus.
func TestJudgeFindings_VerifiedFindingNoPenalty(t *testing.T) {
	judge := scriptedFindingsJudge{
		score:    0.9,
		criteria: map[string]any{"claims_grounded": map[string]any{"score": 9, "reason": "matches the code exactly"}},
		findings: []map[string]any{
			{"index": 1, "path": "internal/vetting/mermaid.go", "line": 40, "status": "verified",
				"why": "read the file: the validation call is exactly as the finding describes"},
		},
	}
	factory := NewJudgeFactory(judge, nil, nil)
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Comments: []ReviewComment{
			{Path: "internal/vetting/mermaid.go", Line: 40, Body: "nit: this validation could use a comment"},
		}},
	}}
	cfg := Config{Rubric: "score 0-10", IsReviewer: true, Threshold: 0.7}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review this PR"}}}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "LGTM.", act, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	cs, ok := v.Criteria[findingsGroundingCriterion]
	if !ok || cs.Score != 0.9 {
		t.Fatalf("%s = %+v, want the judge's own 0.9 untouched - a verified finding must not move it", findingsGroundingCriterion, cs)
	}
	if v.Score < cfg.Threshold {
		t.Fatalf("verdict score = %v, want >= threshold %v", v.Score, cfg.Threshold)
	}
}

// multiFileSpyResult mirrors spyReadResult (judge_test.go) for a stub
// read_file tool that serves different canned content per path, recording
// every path it was asked for.
type multiFileSpyResult struct {
	Content string `json:"content"`
}

func newMultiFileSpyReadTool(t *testing.T, files map[string]string, calls *[]string) tool.Tool {
	t.Helper()
	rt, err := functiontool.New[spyReadArgs, multiFileSpyResult](
		functiontool.Config{Name: "read_file", Description: "Read a text file from your workspace."},
		func(_ adkagent.Context, args spyReadArgs) (multiFileSpyResult, error) {
			*calls = append(*calls, args.Path)
			return multiFileSpyResult{Content: files[args.Path]}, nil
		},
	)
	if err != nil {
		t.Fatalf("spy read tool: %v", err)
	}
	return rt
}

// relatedFileJudge is a deterministic, turn-counted judge: it reads the
// finding's cited file, THEN a related file the finding never mentions (the
// caller), and only after both reads does it submit a verdict contradicting
// the finding on the strength of what the related file showed. Proves #498's
// "do not narrow the judge's view to path:line" requirement - the judge's
// read access reaches a file no finding cited at all.
type relatedFileJudge struct{ turn int32 }

func (j *relatedFileJudge) Name() string { return "related-file-judge" }

func (j *relatedFileJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch atomic.AddInt32(&j.turn, 1) {
		case 1:
			yield(stubCall("read_file", map[string]any{"path": "internal/foo.go"}), nil)
		case 2:
			yield(stubCall("read_file", map[string]any{"path": "internal/bar.go"}), nil)
		default:
			yield(stubCall(submitVerdictTool, map[string]any{
				"score":    0.9,
				"criteria": map[string]any{"claims_grounded": map[string]any{"score": 9, "reason": "locally plausible in isolation"}},
				"findings": []map[string]any{
					{"index": 1, "path": "internal/foo.go", "line": 1, "status": "contradicted",
						"why": "internal/bar.go's caller already guards `input != nil` before calling handleFoo - the deref this finding warns about is unreachable"},
				},
			}), nil)
		}
	}
}

// TestJudgeFindings_ContextDependentRefutationReachesRelatedFile pins test
// case 3 of #498's design: a finding that is accurate AT ITS OWN LINE can
// still be refuted only by a file it never cites (here, the caller that
// already guards the case) - the judge's repo access must not be narrowed to
// the cited path, or it can never reach that file to check.
func TestJudgeFindings_ContextDependentRefutationReachesRelatedFile(t *testing.T) {
	files := map[string]string{
		"internal/foo.go": "func handleFoo(input *Thing) { input.Do() }\n",
		"internal/bar.go": "func caller() { var input *Thing; if input != nil { handleFoo(input) } }\n",
	}
	var calls []string
	readTool := newMultiFileSpyReadTool(t, files, &calls)
	judge := &relatedFileJudge{}
	factory := NewJudgeFactory(judge, []tool.Tool{readTool}, nil)
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "request_changes", Comments: []ReviewComment{
			{Path: "internal/foo.go", Line: 1, Body: "blocking: possible nil deref - input is never checked before use"},
		}},
	}}
	cfg := Config{Rubric: "score 0-10", IsReviewer: true, Threshold: 0.7}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review this PR"}}}

	v, err := runJudgeAgent(t.Context(), factory, cfg, q, "See findings.", act, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if !slices.Contains(calls, "internal/bar.go") {
		t.Fatalf("judge never read internal/bar.go - a file the finding never cited - so its view was effectively narrowed to the cited line: calls=%v", calls)
	}
	if cs := v.Criteria[findingsGroundingCriterion]; cs.Score != 0 {
		t.Fatalf("%s = %+v, want sunk to 0 by the context-dependent contradiction", findingsGroundingCriterion, cs)
	}
}

// reviewGateStub is the worker+judge stub model for
// TestRunGatedRefine_JudgeNeverMutatesStagedReview: the judge ALWAYS
// contradicts the one staged finding (so the gate fails and revises), the
// worker always returns a fixed answer - what matters is what happens to the
// ReviewStage, not the text either side produces.
type reviewGateStub struct {
	workerCalls int32
	judgeCalls  int32
}

func (m *reviewGateStub) Name() string { return "review-gate-stub" }

func (m *reviewGateStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			atomic.AddInt32(&m.judgeCalls, 1)
			yield(stubCall(submitVerdictTool, map[string]any{
				"score":    0.9,
				"criteria": map[string]any{"claims_grounded": map[string]any{"score": 9, "reason": "plausible"}},
				"findings": []map[string]any{
					{"index": 1, "path": "internal/foo.go", "line": 5, "status": "contradicted", "why": "the guard already handles this case"},
				},
			}), nil)
			return
		}
		atomic.AddInt32(&m.workerCalls, 1)
		yield(stubText("Reviewed the change."), nil)
	}
}

// TestRunGatedRefine_JudgeNeverMutatesStagedReview pins test case 4: even
// after a full failing judge round (the contradicted finding above sinks the
// gate, forcing a revise), the staged review's OWN comments - the reviewer's
// source of truth - are exactly what was staged going in. The judge reports;
// it never edits, strips, or reorders.
func TestRunGatedRefine_JudgeNeverMutatesStagedReview(t *testing.T) {
	review := &ReviewStage{}
	review.AddComment("internal/foo.go", 5, "blocking: nil deref on the unchecked input")
	review.SetVerdict("request_changes", "one blocking issue found")
	staged := func() []ReviewComment {
		sd, _ := review.Snapshot()
		return sd.Comments
	}
	before := append([]ReviewComment(nil), staged()...)

	secret, err := NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	RegisterMemSession(secret, MemSession{Review: review})
	defer UnregisterMemSession(secret)
	const token = "planZ/reviewer1"
	RegisterAdvisorThread(token, AdvisorTask{NodeID: "reviewer1", MemSecret: secret})
	defer UnregisterAdvisorThread(token)

	stub := &reviewGateStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "code-reviewer", Model: stub, Description: "reviewer", Instruction: "Review the change.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score 0-10", IsReviewer: true, ReadOnly: true}
	var res GateResult
	node, err := newTestGatedNodeCapture("reviewer-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg, &res)
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

	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Review PR #7.\n\n" + AdvisorThreadMarker(token)}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if res.Passed {
		t.Fatalf("gate result Passed = true, want false (the contradicted finding must sink it)")
	}
	if atomic.LoadInt32(&stub.judgeCalls) < 2 {
		t.Fatalf("judge calls = %d, want at least 2 (fail then re-judge after the revise round)", stub.judgeCalls)
	}

	after := staged()
	if len(after) != len(before) {
		t.Fatalf("staged review comment count changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("staged review comment %d mutated: before=%+v after=%+v", i, before[i], after[i])
		}
	}
}

// TestChangedFilesSection_IncludesNumberedFindingsAndVerdict proves the
// judge prompt carries the staged findings as an explicit NUMBERED list
// (not buried in the review prose) ALONGSIDE the review's overall staged
// verdict, so it can reason about severity/verdict coherence as well as
// verify each claim - both facts land in the same section the judge reads.
func TestChangedFilesSection_IncludesNumberedFindingsAndVerdict(t *testing.T) {
	cfg := probeRepo(t, true)
	cfg.IsReviewer = true
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"review": {Kind: "review", Event: "approve", Comments: []ReviewComment{
			{Path: "internal/foo.go", Line: 5, Body: "blocking (security): unchecked input"},
			{Path: "internal/bar.go", Line: 12, Body: "nit: rename this variable"},
		}},
	}}

	got, _ := changedFilesSection(cfg, act)
	if !strings.Contains(got, "Staged review verdict: approve") {
		t.Fatalf("missing the staged overall verdict:\n%s", got)
	}
	if !strings.Contains(got, "1. internal/foo.go:5 - blocking (security): unchecked input") {
		t.Fatalf("missing numbered finding #1:\n%s", got)
	}
	if !strings.Contains(got, "2. internal/bar.go:12 - nit: rename this variable") {
		t.Fatalf("missing numbered finding #2:\n%s", got)
	}
	if !strings.Contains(got, "findings` array") {
		t.Fatalf("missing the submit_verdict findings-array instruction:\n%s", got)
	}
}
