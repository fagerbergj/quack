package vetting

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
)

// resumeWorkerModel pauses on the first turn (a request_input call) and, once it
// sees the FunctionResponse answering it, writes the final answer. This drives a
// REAL worker llmagent through the trust gate, so the test proves the actual
// resume mechanic — the worker rebuilding its request from its persisted session
// (open call + appended answer) and continuing — not just the executor schedule.
type resumeWorkerModel struct{}

func (resumeWorkerModel) Name() string { return "resume-worker" }

func (resumeWorkerModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	answered := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "request_input" {
				answered = true
			}
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		if answered {
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Final answer: visit the north in spring."}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{ID: "ri1", Name: "request_input", Args: map[string]any{"questions": []any{"Which region?"}}},
			}}},
			TurnComplete: true,
		}, nil)
	}
}

// TestResumeContinuesPausedWorker is the integration test for M5b resume: a real
// gated worker pauses on request_input mid-DAG, then exec.Resume delivers the
// answer to its persisted session and the worker continues to a vetted answer.
func TestResumeContinuesPausedWorker(t *testing.T) {
	riTool, err := functiontool.New[riArgs, riResult](
		functiontool.Config{Name: "request_input", Description: "ask the user a question", IsLongRunning: true},
		func(tc adkagent.ToolContext, _ riArgs) (riResult, error) {
			tc.Actions().SkipSummarization = true
			return riResult{Status: "pending"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := llmagent.New(llmagent.Config{
		Name:        "web-researcher",
		Description: "researches the web",
		Model:       resumeWorkerModel{},
		Instruction: "research",
		Tools:       []tool.Tool{riTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	gated, err := NewGatedAgent(worker, nil, scriptedJudge(judgeTurn{score: 0.9, feedback: "ok"}), Config{JudgeRounds: 1, Threshold: 0.7, Rubric: "r"})
	if err != nil {
		t.Fatal(err)
	}

	// One shared session service so the node's paused session persists between the
	// Execute (pause) and Resume (continue) runs — keyed by plan.ID:node.ID.
	sessions := session.InMemoryService()
	exec := dag.NewExecutor(sessions, map[string]adkagent.Agent{"web-researcher": gated}, nil, 1)
	plan := dag.Plan{ID: "p1", UserMessage: "plan a trip", Nodes: []dag.Node{
		{ID: "n1", AgentName: "web-researcher", Task: "recommend a region and season"},
	}}

	// Phase 1: run until the node pauses on request_input.
	var callID string
	for ev, err := range exec.Execute(context.Background(), plan, "u", map[string]string{}) {
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if d, ok := ev.Data.(stream.NodeWaitingData); ok {
			callID = d.CallID
		}
	}
	if callID == "" {
		t.Fatal("expected the node to pause with node_waiting")
	}

	// Phase 2: resume with the answer; the worker should continue and the gate
	// should vet a real answer (no fresh task — the FunctionResponse drives it).
	answer := &genai.Content{Role: "user", Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: callID, Name: "request_input", Response: map[string]any{"answers": []any{"north"}}},
	}}}
	outputs := map[string]string{}
	gotDone := false
	for ev, err := range exec.Resume(context.Background(), plan, "u", outputs, nil, nil, "n1", answer) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if _, ok := ev.Data.(stream.NodeDoneData); ok {
			gotDone = true
		}
	}
	if !gotDone {
		t.Fatal("expected node_done after resume")
	}
	if !strings.Contains(outputs["n1"], "Final answer") {
		t.Errorf("resumed node output = %q, want the post-answer text", outputs["n1"])
	}
}
