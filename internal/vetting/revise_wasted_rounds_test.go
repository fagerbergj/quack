package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	workflowagent "google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// emptyReviseModel: draft answers, judge always fails, revise returns "" -
// the shape an ACP round takes when it ends on a tool call with no trailing
// text (finalSpec answer_len 0).
type emptyReviseModel struct {
	workerCalls  int
	judgeCalls   int
	judgePrompts []string
}

func (m *emptyReviseModel) Name() string { return "stub-empty-revise" }

func (m *emptyReviseModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeCalls++
			m.judgePrompts = append(m.judgePrompts, stubAllText(req))
			yield(stubCall(submitVerdictTool, map[string]any{"score": 0.4, "feedback": "tighten the claims"}), nil)
			return
		}
		m.workerCalls++
		if strings.Contains(stubAllText(req), "Verdict:") {
			yield(stubText(""), nil) // revise round produced no text
			return
		}
		yield(stubText("This is the initial draft answer, long enough to be judged."), nil)
	}
}

// TestEmptyReviseStopsRoundLoop proves an empty revise answer ends the round
// loop keeping the current verdict instead of re-judging byte-identical
// content (node.go's revise guard at the end of the round loop).
func TestEmptyReviseStopsRoundLoop(t *testing.T) {
	stub := &emptyReviseModel{}
	worker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Model: stub, Description: "researcher", Instruction: "Answer."})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JudgeRounds: 3, Threshold: 0.7, Rubric: "score the answer 0-10"}
	node, err := newTestGatedNode("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workflowagent.New(workflowagent.Config{Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "What is the capital of France?"}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Draft, then one revise that comes back empty: the loop must stop right
	// there, not burn the remaining JudgeRounds re-judging the same answer.
	if stub.judgeCalls != 1 {
		t.Fatalf("judge calls = %d, want exactly 1 (round loop must stop after the first empty revise, not run all %d JudgeRounds)", stub.judgeCalls, cfg.JudgeRounds)
	}
	if stub.workerCalls != 2 {
		t.Fatalf("worker calls = %d, want 2 (1 draft + 1 empty revise)", stub.workerCalls)
	}
}
