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

// establishThenEmptyStub simulates an ACP round that DOES establish a real
// transport session (SetAdvisorThreadSessionID, exactly what
// internal/acp.Agent.round does after session/new or session/load succeeds)
// and then produces an empty draft, forcing the node to fail. Proves the
// node's real session id is captured even on the ErrNodeEmpty exit path -
// not just on success.
type establishThenEmptyStub struct{}

func (establishThenEmptyStub) Name() string { return "establishThenEmptyStub" }
func (establishThenEmptyStub) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9}), nil)
			return
		}
		if token, ok := vetting.ParseAdvisorThread(gUserText(req)); ok {
			vetting.SetAdvisorThreadSessionID(token, "acp-real-session-on-failure")
		}
		yield(gText(""), nil)
	}
}

// TestNodeFailed_CarriesRealContextIDEstablishedBeforeFailing: an ACP
// node's real session id, once established, must reach the node_failed
// event's ContextID - runlog then persists it onto the dag_node record, so
// a later reuse's session/load gets something real instead of the
// mint-time placeholder.
func TestNodeFailed_CarriesRealContextIDEstablishedBeforeFailing(t *testing.T) {
	stub := establishThenEmptyStub{}
	ag, err := llmagent.New(llmagent.Config{Name: "w", Model: stub, Description: "w", Instruction: "ROLE:w Answer."})
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(session.InMemoryService(), map[string]adkagent.Agent{"w": ag}, map[string]model.LLM{"w": stub},
		vetting.NewJudgeFactory(stub, nil, nil), func(string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} }, nil)
	plan := Plan{ID: "t", UserMessage: "x", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "do it"}}}

	events, _ := runPlanSSE(t, ex, plan, "chat")
	var found bool
	for _, ev := range events {
		if d, ok := ev.Data.(stream.NodeFailedData); ok && d.NodeID == "n1" {
			found = true
			if d.ContextID != "acp-real-session-on-failure" {
				t.Errorf("node_failed.ContextID = %q, want the real session id established before the failure", d.ContextID)
			}
		}
	}
	if !found {
		t.Fatal("no node_failed event observed for n1")
	}
}
