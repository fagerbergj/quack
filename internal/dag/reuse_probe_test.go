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

type probeStub struct {
	mu    sync.Mutex
	calls int
	saw   bool
}

func (*probeStub) Name() string { return "probeStub" }

func (s *probeStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		s.calls++
		n := s.calls
		if n > 1 && strings.Contains(atAllText(req), "RUN-ONE-OUTPUT") {
			s.saw = true
		}
		s.mu.Unlock()
		if n == 1 {
			yield(atText("RUN-ONE-OUTPUT"), nil)
			return
		}
		yield(atText("RUN-TWO-OUTPUT"), nil)
	}
}

type passJudge struct{}

func (*passJudge) Name() string { return "passJudge" }
func (*passJudge) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(atCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
	}
}

// TestNodeOverA2A_ReusedAcrossSeparateRunPlanAsGraphInvocations probes whether
// two SEPARATE top-level RunPlanAsGraph calls (simulating two separate plan
// executions on different chat turns) for the SAME node identity see the
// same remote session history, when nothing deletes the session in between.
func TestNodeOverA2A_ReusedAcrossSeparateRunPlanAsGraphInvocations(t *testing.T) {
	sessions := session.InMemoryService()
	stub := &probeStub{}
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

	run := func(chatID string) string {
		plan := dag.Plan{ID: "p-" + chatID, UserMessage: "x", Nodes: []dag.Node{
			{ID: "n1", AgentName: "solo", Task: "Write the thing.", Rubric: "detailed"},
		}}
		ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"solo": client}, nil,
			vetting.NewJudgeFactory(&passJudge{}, nil, nil),
			func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
		outputs := map[string]string{}
		content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "x"}}}
		if _, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", chatID, content,
			func(stream.SSEEvent, error) bool { return true }, outputs, nil); err != nil {
			t.Fatalf("run %s: %v", chatID, err)
		}
		return outputs["n1"]
	}

	if got := run("c1"); !strings.Contains(got, "RUN-ONE-OUTPUT") {
		t.Fatalf("run 1 output = %q", got)
	}
	if got := run("c1"); !strings.Contains(got, "RUN-TWO-OUTPUT") {
		t.Fatalf("run 2 output = %q", got)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.saw {
		t.Error("second RunPlanAsGraph invocation did not see the first's history - cross-turn reuse via a stable A2A contextID is broken")
	}
}
