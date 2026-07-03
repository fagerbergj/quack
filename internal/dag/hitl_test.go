package dag

import (
	"context"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

const testAskTool = "ask_user"

type askArgs struct {
	Question string `json:"question"`
}
type askResult struct {
	Status string `json:"status"`
}

// newAskTool is a minimal long-running "ask the user" tool (like get_user_choice
// but built inline to avoid the tools→dag import cycle).
func newAskTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New[askArgs, askResult](
		functiontool.Config{Name: testAskTool, Description: "Ask the user a question."},
		func(tc adkagent.Context, a askArgs) (askResult, error) {
			// Native tool-level HITL: record a pending confirmation + halt the loop.
			if err := tc.RequestConfirmation(a.Question, nil); err != nil {
				return askResult{}, err
			}
			return askResult{Status: "pending"}, nil
		})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return tl
}

// hitlStub: as a worker it calls the long-running ask_user tool (→ HITL pause); as
// a judge it passes.
type hitlStub struct {
	mu          sync.Mutex
	workerCalls int
}

func (*hitlStub) Name() string { return "hitlStub" }

func (s *hitlStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		s.mu.Lock()
		s.workerCalls++
		s.mu.Unlock()
		yield(gCall(testAskTool, map[string]any{"question": "which direction?"}), nil)
	}
}

// TestHITL_WorkerPausesNode: a worker that calls a long-running tool parks its node
// instead of failing — WithRaiseOnWait turns the unresolved tool into an interrupt
// that runDAG→execNode swallows into a pause. The node emits no answer and the tool
// call surfaces to the client.
func TestHITL_WorkerPausesNode(t *testing.T) {
	stub := &hitlStub{}
	ag, err := llmagent.New(llmagent.Config{
		Name: "blk", Model: stub, Description: "blk", Instruction: "ROLE:blk Answer.",
		Tools: []tool.Tool{newAskTool(t)},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"blk": ag}, nil, nil,
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "blk", Task: "do it"}}}

	events, outputs := runPlanSSE(t, ex, plan, "s")

	if out := outputs["n1"]; out != "" {
		t.Errorf("paused node should have no output, got %q", out)
	}
	var sawAsk, sawDone bool
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.AgentToolCallData:
			if d.Name == testAskTool {
				sawAsk = true
			}
		case stream.NodeDoneData:
			if d.NodeID == "n1" {
				sawDone = true
			}
		}
	}
	if !sawAsk {
		t.Errorf("expected the ask_user tool call to surface in events")
	}
	if sawDone {
		t.Errorf("node n1 should be paused, not done")
	}
	if stub.workerCalls == 0 {
		t.Errorf("worker never ran")
	}
}
