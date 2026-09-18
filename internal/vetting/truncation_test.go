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
	if want := "It was a dark and stormy night."; final != want {
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
	if final != "ABC" {
		t.Errorf("final answer = %q, want %q", final, "ABC")
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
	if final != "DEF" {
		t.Errorf("final answer = %q, want %q (round 2's revision, still cut off)", final, "DEF")
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
