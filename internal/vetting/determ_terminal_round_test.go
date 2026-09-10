package vetting

import (
	"context"
	"iter"
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

// alwaysPassModel: worker answers, judge awards full marks on every criterion
// it is asked about - so any failure below can only come from a deterministic
// criterion, never from the judge model.
type alwaysPassModel struct{ workerCalls, judgeCalls int }

func (m *alwaysPassModel) Name() string { return "always-pass" }

func (m *alwaysPassModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stubHasTool(req, submitVerdictTool) {
			m.judgeCalls++
			yield(stubCall(submitVerdictTool, map[string]any{"score": 3.0, "feedback": ""}), nil)
			return
		}
		m.workerCalls++
		yield(stubText("A thorough answer with plenty of substance so sufficient_length passes easily. "+
			"It restates the finding, explains the mechanism, and closes with a recommendation."), nil)
	}
}

// TestDeterministicFailSkipsJudgeOnTerminalRound proves a deterministic
// criterion already below threshold (RequireRetrieval with zero retrieval
// activity here) decides the terminal round without paying for a judge model call whose feedback nothing would ever consume.
func TestDeterministicFailSkipsJudgeOnTerminalRound(t *testing.T) {
	stub := &alwaysPassModel{}
	worker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Model: stub, Description: "r", Instruction: "Answer."})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score 0-3", RequireRetrieval: true}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stub, NewJudgeFactory(stub, nil, nil), cfg, &res)
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
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research X."}}}
	for _, err := range r.Run(t.Context(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Round loop runs JudgeRounds+1 rounds (2 revises + 1 terminal). Every
	// revise round still needs the judge to produce feedback the worker can
	// act on, but the terminal round's judge call is skipped once a deterministic criterion has already failed - JudgeRounds calls, not JudgeRounds+1.
	if stub.judgeCalls != cfg.JudgeRounds {
		t.Fatalf("judge model rounds run = %d, want %d (terminal round must skip the judge once grounded_in_retrieval already fails)", stub.judgeCalls, cfg.JudgeRounds)
	}
	if res.Passed {
		t.Fatal("gate must fail: RequireRetrieval with zero retrieval activity never passes")
	}
}
