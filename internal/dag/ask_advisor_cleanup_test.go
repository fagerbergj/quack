package dag_test

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/vetting"
)

// oneShotAdvisorStub consults ask_advisor once, then answers.
type oneShotAdvisorStub struct{ calls int }

func (*oneShotAdvisorStub) Name() string { return "oneShotAdvisorStub" }

func (s *oneShotAdvisorStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if atHasTool(req, "submit_verdict") {
			yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.calls++
		if s.calls == 1 {
			yield(atCall("ask_advisor", map[string]any{"request": "how should I scope this?"}), nil)
			return
		}
		yield(atText("final answer"), nil)
	}
}

// TestAskAdvisor_SessionReapedWhenNodeDone is a regression test for the ADK
// audit's A2 finding: an ask_advisor consult (internal/tools.
// NewAskAdvisorTool) leaves a Postgres session under AppName="quack-advisor"
// keyed by the node's advisor thread token - a second orphaned-session class
// alongside the A2A worker one, never reaped. newGatedNode must now delete
// that session (vetting.AdvisorSessionApp/AdvisorSessionUser/AdvisorSessionID)
// as soon as the node finishes, the same moment it already unregisters the
// in-memory advisor thread.
func TestAskAdvisor_SessionReapedWhenNodeDone(t *testing.T) {
	stub := &oneShotAdvisorStub{}
	advisor := &recordingAdvisor{}
	sessions := session.InMemoryService()
	tl := newAdvisorTool(t, advisor, sessions)
	worker, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{tl},
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "blk", Task: "do it", Rubric: "must be thorough"},
	}}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	paused, outputs, _ := runGraph(t, worker, stub, sessions, plan, content, nil)
	if paused {
		t.Fatal("run should not pause")
	}
	if outputs["n1"] != "final answer" {
		t.Fatalf("n1 output = %q, want %q", outputs["n1"], "final answer")
	}
	advisor.mu.Lock()
	calls := len(advisor.prompts)
	advisor.mu.Unlock()
	if calls != 1 {
		t.Fatalf("advisor called %d times, want 1", calls)
	}

	token := vetting.AdvisorThreadToken(plan.ID, "n1")
	resp, err := sessions.Get(context.Background(), &session.GetRequest{
		AppName: vetting.AdvisorSessionApp, UserID: vetting.AdvisorSessionUser, SessionID: vetting.AdvisorSessionID(token),
	})
	if err == nil && resp != nil && resp.Session != nil {
		t.Fatalf("advisor session %q still present after node completed, want reaped", vetting.AdvisorSessionID(token))
	}
}
