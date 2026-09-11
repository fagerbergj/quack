package dag_test

import (
	"context"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"iter"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// seenACPSessionIDStub reads back whatever ACP session id the gate seeded on
// this node's AdvisorTask - it parses the advisor-thread token out of its own
// prompt (the same marker the real ACP transport looks for) and looks the
// live registration up while it is still registered.
type seenACPSessionIDStub struct {
	mu   sync.Mutex
	seen string
	ok   bool
}

func (*seenACPSessionIDStub) Name() string { return "seenACPSessionIDStub" }

func (s *seenACPSessionIDStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if token, ok := vetting.ParseAdvisorThread(atAllText(req)); ok {
			if at, ok := vetting.LookupAdvisorThread(token); ok {
				s.mu.Lock()
				s.seen, s.ok = at.ACPSessionID, true
				s.mu.Unlock()
			}
		}
		yield(atText("done"), nil)
	}
}

type alwaysPassJudge struct{}

func (*alwaysPassJudge) Name() string { return "alwaysPassJudge" }
func (*alwaysPassJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// TestNewGatedNode_SeedsACPSessionIDFromResumedFrom is the missing link
// between reuse (list_nodes/create_plan naming an existing node id) and ACP
// resume (session/load): a node's AdvisorTask must be registered with
// ACPSessionID already set to Node.ResumedFrom, before its first round ever
// dispatches - internal/acp's own resolveNode reads exactly that field as
// this round's priorSessionID.
func TestNewGatedNode_SeedsACPSessionIDFromResumedFrom(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &seenACPSessionIDStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "solo", Task: "continue the work.", ResumedFrom: "prior-acp-session-xyz"},
	}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": worker}, nil,
		vetting.NewJudgeFactory(&alwaysPassJudge{}, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.ok {
		t.Fatal("worker never saw a parseable advisor-thread token - can't check ACPSessionID")
	}
	if stub.seen != "prior-acp-session-xyz" {
		t.Errorf("AdvisorTask.ACPSessionID = %q, want the node's ResumedFrom value", stub.seen)
	}
}

// TestNewGatedNode_FreshNodeHasNoACPSessionID: a node with no ResumedFrom
// (never reused) must not accidentally seed a stale/guessed session id.
func TestNewGatedNode_FreshNodeHasNoACPSessionID(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &seenACPSessionIDStub{}
	worker, err := llmagent.New(llmagent.Config{
		Name: "solo", Model: stub, Description: "solo", Instruction: "ROLE:solo Answer.",
	})
	if err != nil {
		t.Fatalf("worker agent: %v", err)
	}

	plan := dag.Plan{ID: "p", UserMessage: "x", Nodes: []dag.Node{
		{ID: "n1", AgentName: "solo", Task: "do the work."},
	}}
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": worker}, nil,
		vetting.NewJudgeFactory(&alwaysPassJudge{}, nil, nil),
		func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)

	outputs := map[string]string{}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
	if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", "c1", content,
		func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.ok {
		t.Fatal("worker never saw a parseable advisor-thread token")
	}
	if stub.seen != "" {
		t.Errorf("AdvisorTask.ACPSessionID = %q, want empty for a fresh node", stub.seen)
	}
}

