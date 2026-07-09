package dag

import (
	"context"
	"iter"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// emptyStub returns an empty worker draft (→ ErrNodeEmpty), so the gated node
// fails; the judge always passes (never reached on an empty draft).
type emptyStub struct{}

func (emptyStub) Name() string { return "emptyStub" }
func (emptyStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9}), nil)
			return
		}
		yield(gText(""), nil)
	}
}

// TestExecute_EmptyNode_FailsLoud: a node that produces no answer surfaces as a
// loud node_failed (not a quiet node_done); the run still completes.
func TestExecute_EmptyNode_FailsLoud(t *testing.T) {
	stub := emptyStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": ag}, map[string]model.LLM{"w": stub},
		vetting.NewJudgeFactory(stub, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	var failed, done bool
	events, _ := runPlanSSE(t, ex, plan, "chat")
	for _, ev := range events {
		switch d := ev.Data.(type) {
		case stream.NodeFailedData:
			if d.NodeID == "n1" {
				failed = true
			}
		case stream.NodeDoneData:
			if d.NodeID == "n1" {
				done = true
			}
		}
	}
	if !failed {
		t.Error("empty node should surface as node_failed")
	}
	if done {
		t.Error("empty node should NOT emit a quiet node_done")
	}
}
