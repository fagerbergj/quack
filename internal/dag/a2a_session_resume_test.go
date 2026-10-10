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

// reviseStub drafts once, then revises; its second request must still carry the first
// draft, which only happens if the node resumed the SAME remote A2A contextID.
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

// The per-node identity must stay stable across judge/revise rounds so the revise
// dispatch lands back in the node's own remote A2A session.
func TestNodeOverA2A_ResumesItsOwnRemoteSessionAcrossRounds(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &reviseStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}
	srv, err := quackagent.Serve(worker, sessions, nil, nil, quackagent.Compaction{}, "", nil)
	if err != nil {
		t.Fatalf("a2a serve: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	client, err := srv.ClientForNode("test-node", "test-ctx")
	if err != nil {
		t.Fatalf("a2a client: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"},
	}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": client}, nil,
		vetting.NewJudgeFactory(failOnceJudge(0.2, "add detail"), nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

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
