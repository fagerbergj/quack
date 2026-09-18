package vetting

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"unicode/utf8"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// queuedTurn is one scripted worker reply for truncationStubModel.
type queuedTurn struct {
	text   string
	finish genai.FinishReason
}

// truncationStubModel drives MAX_TOKENS/STOP scripted worker turns to test
// the gate's cut-off continuation loop; a submit_verdict-bearing call is the
// judge and always passes, so a round's judging never depends on how many
// continuations it took to assemble the answer.
type truncationStubModel struct {
	queue       []queuedTurn
	workerCalls int
	judgeCalls  int
	// errAtCall: 1-based worker-call index that fails instead of replying (0 = never).
	errAtCall int
}

func (m *truncationStubModel) Name() string { return "truncation-stub" }

func (m *truncationStubModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeCalls++
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": "fine"}), nil)
			return
		}
		i := m.workerCalls
		m.workerCalls++
		if m.errAtCall == i+1 {
			yield(nil, errors.New("simulated transport error"))
			return
		}
		t := queuedTurn{text: "(unexpected extra worker call)", finish: genai.FinishReasonStop}
		if i < len(m.queue) {
			t = m.queue[i]
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: t.text}}},
			FinishReason: t.finish,
			TurnComplete: true,
		}, nil)
	}
}

// runTruncationNode drives one RunGatedRefine dispatch through the real ADK
// runner (mirrors newTestGatedNode's siblings above), returning the final
// answer and the captured GateResult.
func runTruncationNode(t *testing.T, stub *truncationStubModel) (string, GateResult) {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{
		Name: "web-researcher", Model: stub, Description: "researcher",
		Instruction: "Answer the question.",
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "score the answer 0-10"}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg, &res)
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
	}
	return final, res
}

// TestRunGatedRefine_TruncatedAnswerGetsOneContinuation is the #3f4d9045 case:
// a MAX_TOKENS draft is continued once, the remainder is appended verbatim
// (never repeated), and the round is judged normally once complete.
func TestRunGatedRefine_TruncatedAnswerGetsOneContinuation(t *testing.T) {
	stub := &truncationStubModel{queue: []queuedTurn{
		{text: "It was a dark and stormy ni", finish: genai.FinishReasonMaxTokens},
		{text: "ght.", finish: genai.FinishReasonStop},
	}}
	final, res := runTruncationNode(t, stub)
	// truncationSeam inserts "\n" since the continuation doesn't open with whitespace.
	if want := "It was a dark and stormy ni\nght."; final != want {
		t.Errorf("final answer = %q, want %q", final, want)
	}
	if stub.workerCalls != 2 {
		t.Errorf("worker calls = %d, want 2 (draft + 1 continuation)", stub.workerCalls)
	}
	if stub.judgeCalls != 1 {
		t.Errorf("judge calls = %d, want 1 (judged once, after continuation)", stub.judgeCalls)
	}
	if !res.Passed {
		t.Errorf("GateResult.Passed = false, want true (complete answer, passing judge score)")
	}
}

// TestRunGatedRefine_TruncatedAnswerGetsTwoContinuations: two consecutive
// MAX_TOKENS turns are both continued before a STOP turn completes the answer.
func TestRunGatedRefine_TruncatedAnswerGetsTwoContinuations(t *testing.T) {
	stub := &truncationStubModel{queue: []queuedTurn{
		{text: "A", finish: genai.FinishReasonMaxTokens},
		{text: "B", finish: genai.FinishReasonMaxTokens},
		{text: "C", finish: genai.FinishReasonStop},
	}}
	final, res := runTruncationNode(t, stub)
	if final != "A\nB\nC" {
		t.Errorf("final answer = %q, want %q", final, "A\\nB\\nC")
	}
	if stub.workerCalls != 3 {
		t.Errorf("worker calls = %d, want 3 (draft + 2 continuations)", stub.workerCalls)
	}
	if !res.Passed {
		t.Errorf("GateResult.Passed = false, want true (complete answer, passing judge score)")
	}
}

// TestRunGatedRefine_ExhaustedContinuationsFailClosed: three consecutive
// MAX_TOKENS turns burn the round's continuation budget (2), and the
// budget-exhausted revision round does too - proving complete_output=0 fails
// the round by weakest-link no matter what the judge would have scored it.
func TestRunGatedRefine_ExhaustedContinuationsFailClosed(t *testing.T) {
	stub := &truncationStubModel{queue: []queuedTurn{
		{text: "A", finish: genai.FinishReasonMaxTokens}, // round 1 draft
		{text: "B", finish: genai.FinishReasonMaxTokens}, // round 1 continuation 1
		{text: "C", finish: genai.FinishReasonMaxTokens}, // round 1 continuation 2 (budget exhausted)
		{text: "D", finish: genai.FinishReasonMaxTokens}, // round 2 revise
		{text: "E", finish: genai.FinishReasonMaxTokens}, // round 2 continuation 1
		{text: "F", finish: genai.FinishReasonMaxTokens}, // round 2 continuation 2 (budget exhausted, terminal round)
	}}
	final, res := runTruncationNode(t, stub)
	if final != "D\nE\nF" {
		t.Errorf("final answer = %q, want %q (round 2's revision, still cut off)", final, "D\\nE\\nF")
	}
	if stub.workerCalls != 6 {
		t.Errorf("worker calls = %d, want 6 (2 rounds x (1 producing call + 2 continuations))", stub.workerCalls)
	}
	if res.Passed {
		t.Errorf("GateResult.Passed = true, want false - a truncated answer must never pass")
	}
	if res.Score != 0 {
		t.Errorf("GateResult.Score = %v, want 0 (complete_output=0 forces the weakest-link score)", res.Score)
	}
}

// TestRunGatedRefine_CompleteAnswerNeverContinues: a normal STOP-finished
// draft never enters the continuation path at all.
func TestRunGatedRefine_CompleteAnswerNeverContinues(t *testing.T) {
	stub := &truncationStubModel{queue: []queuedTurn{
		{text: "The capital of France is Paris.", finish: genai.FinishReasonStop},
	}}
	final, res := runTruncationNode(t, stub)
	if final != "The capital of France is Paris." {
		t.Errorf("final answer = %q, want the draft unchanged", final)
	}
	if stub.workerCalls != 1 {
		t.Errorf("worker calls = %d, want 1 (no continuation)", stub.workerCalls)
	}
	if !res.Passed {
		t.Errorf("GateResult.Passed = false, want true")
	}
}

// TestRunGatedRefine_ContinuationCallFailureStillJudges: a transport error on
// the continuation call itself (not a repeat-guard abort) doesn't hang or
// retry forever - it judges the still-cut-off answer as truncated instead.
func TestRunGatedRefine_ContinuationCallFailureStillJudges(t *testing.T) {
	stub := &truncationStubModel{
		queue: []queuedTurn{
			{text: "cut off mid", finish: genai.FinishReasonMaxTokens},
		},
		errAtCall: 2, // the continuation call
	}
	runTruncationNode(t, stub) // must not hang or error the run
	if stub.workerCalls != 3 {
		t.Errorf("worker calls = %d, want 3 (draft + 1 failed continuation, no retry + round 2's revise)", stub.workerCalls)
	}
	if stub.judgeCalls != 2 {
		t.Errorf("judge calls = %d, want 2 - a continuation failure must still reach judging, round 1 fails, round 2 revises", stub.judgeCalls)
	}
}

// TestWorkerRunFinishReason_NilSession: a defensive guard, not a real path -
// checkTruncation always has a live session, but the helper must not panic.
func TestWorkerRunFinishReason_NilSession(t *testing.T) {
	if got := workerRunFinishReason(nil, "inv", "node", "run"); got != genai.FinishReasonUnspecified {
		t.Errorf("workerRunFinishReason(nil session) = %v, want Unspecified", got)
	}
}

// TestBuildTruncationContinuationPrompt_LongAnswerQuotesOnlyTheTail proves the
// worker is shown its last ~200 chars, not the whole (possibly huge) answer.
func TestBuildTruncationContinuationPrompt_LongAnswerQuotesOnlyTheTail(t *testing.T) {
	answer := strings.Repeat("x", 500) + "THE_TAIL_END"
	got := buildTruncationContinuationPrompt(answer)
	if !strings.Contains(got, "THE_TAIL_END") {
		t.Fatalf("prompt missing the answer's actual tail:\n%s", got)
	}
	if strings.Contains(got, strings.Repeat("x", truncationTailChars+1)) {
		t.Errorf("prompt quoted more than truncationTailChars of the answer")
	}
}

// TestBuildTruncationContinuationPrompt_RuneSafeTail: the byte cutoff lands
// inside "日"'s 3-byte encoding - the trailing partial rune must be dropped,
// not left to corrupt the quoted tail (valid UTF-8 in, valid UTF-8 out).
func TestBuildTruncationContinuationPrompt_RuneSafeTail(t *testing.T) {
	answer := "日" + strings.Repeat("x", truncationTailChars-1) // len = 3 + 199 = 202
	got := buildTruncationContinuationPrompt(answer)
	if !utf8.ValidString(got) {
		t.Fatalf("prompt is not valid UTF-8 after tail-trimming:\n%q", got)
	}
	if strings.Contains(got, "日") {
		t.Errorf("prompt kept a rune whose bytes were cut mid-sequence, want it dropped entirely:\n%s", got)
	}
}

// TestTruncationSeam: a "\n" seam unless the continuation already opens with
// whitespace, avoiding a doubled seam.
func TestTruncationSeam(t *testing.T) {
	cases := []struct{ cont, want string }{
		{"ght.", "\n"},
		{" the rest.", ""},
		{"\nthe rest.", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := truncationSeam(c.cont); got != c.want {
			t.Errorf("truncationSeam(%q) = %q, want %q", c.cont, got, c.want)
		}
	}
}
