package dag_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"iter"

	quackagent "github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// reviseStub drafts once, then (after the judge's fail) revises. Its second
// draft asserts the remote session CONTINUED: the revise round's request must
// still carry the first draft, which only happens when the node resumed the SAME
// remote A2A contextID (multi-turn dispatch). A per-node identity must keep this
// working - unique across nodes, but stable across a node's rounds.
type reviseStub struct {
	mu       sync.Mutex
	calls    int
	sawDraft bool // the revise request carried the first draft back
}

func (*reviseStub) Name() string { return "reviseStub" }

func (s *reviseStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		if n > 1 && strings.Contains(atAllText(req), "DRAFT-ONE") {
			s.sawDraft = true
		}
		s.mu.Unlock()
		if n == 1 {
			yield(atText("DRAFT-ONE"), nil)
			return
		}
		yield(atText("FINAL-TWO"), nil)
	}
}

// failThenPassJudge fails the first verdict (forcing a revise round) and passes
// the second.
type failThenPassJudge struct {
	mu     sync.Mutex
	rounds int
}

func (*failThenPassJudge) Name() string { return "failThenPassJudge" }
func (j *failThenPassJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		j.mu.Lock()
		j.rounds++
		n := j.rounds
		j.mu.Unlock()
		if n == 1 {
			yield(atCall("submit_verdict", map[string]any{"score": 0.2, "feedback": "add detail"}), nil)
			return
		}
		yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// TestNodeOverA2A_ResumesItsOwnRemoteSessionAcrossRounds: the per-node identity
// must stay STABLE across a node's judge/revise rounds - the revise dispatch has
// to land back in the node's own remote A2A session (carrying the draft it is
// revising), not a fresh one.
func TestNodeOverA2A_ResumesItsOwnRemoteSessionAcrossRounds(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &reviseStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	srv, err := quackagent.Serve(worker, sessions, nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node")
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"},
	}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": client}, nil,
		vetting.NewJudgeFactory(&failThenPassJudge{}, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := outputs["n1"]; !strings.Contains(got, "FINAL-TWO") {
		t.Fatalf("n1 output = %q, want the revised answer", got)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.calls < 2 {
		t.Fatalf("worker ran %d times, want a draft + a revise round", stub.calls)
	}
	if !stub.sawDraft {
		t.Error("the revise round did not see the node's own first draft - the node did not resume its own remote A2A session")
	}
}
