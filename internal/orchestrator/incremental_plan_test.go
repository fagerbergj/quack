// A plan grown from A to A+B across two execute() calls in one orchestrator turn.
package orchestrator

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/stream"
)

// partialPlanCall declares no delivery, unlike planCall, so the first execute is partial.
func partialPlanCall() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{"assignments": []any{map[string]any{
		"agent": "web-researcher", "task": "research the thing",
	}}})
}

// editPlanAddB adds B depending on aNodeID and declares delivery.
func editPlanAddB(planID, aNodeID string) *model.LLMResponse {
	return stubCall("edit_plan", map[string]any{
		"plan_id": planID,
		"assignments": []any{map[string]any{
			"agent": "web-researcher", "task": "write up what A found", "depends_on": []any{aNodeID},
		}},
		"delivery": map[string]any{"kind": "comment"},
	})
}

// createPlanNodeID finds create_plan's minted node id for agent in req's
// history - the id a later edit_plan's depends_on needs to reference A.
func createPlanNodeID(req *model.LLMRequest, agent string) (string, bool) {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil || p.FunctionResponse.Name != "create_plan" {
				continue
			}
			raw, ok := p.FunctionResponse.Response["assignments"]
			if !ok {
				continue
			}
			list, ok := raw.([]any)
			if !ok {
				continue
			}
			for _, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if a, _ := m["agent"].(string); a != agent {
					continue
				}
				if id, ok := m["node_id"].(string); ok && id != "" {
					return id, true
				}
			}
		}
	}
	return "", false
}

// executedPlanIDs collects every plan_id an execute call named.
func executedPlanIDs(req *model.LLMRequest) []string {
	var out []string
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == "execute" {
				if id, ok := p.FunctionCall.Args["plan_id"].(string); ok {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// hasFunctionCall makes edit_plan fire once: edit_plan never adds to the execute count.
func hasFunctionCall(req *model.LLMRequest, name string) bool {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil && p.FunctionCall.Name == name {
				return true
			}
		}
	}
	return false
}

// incrementalStub: create_plan(A) -> execute (partial) -> edit_plan(add B, delivery) -> execute.
// orchStub's auto-execute would also fire on the second plan_id, so it scripts the growth step.
type incrementalStub struct {
	orchStub
}

func (s *incrementalStub) GenerateContent(ctx context.Context, req *model.LLMRequest, isStream bool) iter.Seq2[*model.LLMResponse, error] {
	if stubHasTool(req, "submit_verdict") || !stubHasTool(req, "create_plan") {
		return s.orchStub.GenerateContent(ctx, req, isStream)
	}
	execs := executedPlanIDs(req)
	switch {
	case len(execs) == 0:
		// No execute call yet: author the first assignment (or - once its
		// response is in history - fall through to orchStub's auto-execute).
		return s.orchStub.GenerateContent(ctx, req, isStream)
	case !hasFunctionCall(req, "edit_plan"):
		// Gated on edit_plan, not len(execs), which edit_plan never changes.
		aID, ok := createPlanNodeID(req, "web-researcher")
		if !ok {
			return s.orchStub.GenerateContent(ctx, req, isStream)
		}
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(editPlanAddB(execs[0], aID), nil)
		}
	default:
		// edit_plan just grew the SAME plan_id - call execute again to deliver it.
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(executeCall(execs[0]), nil)
		}
	}
}

func countEvent(evs []stream.SSEEvent, name string) int {
	n := 0
	for _, ev := range evs {
		if ev.Name == name {
			n++
		}
	}
	return n
}

// The first execute's result leads to an edit_plan that adds B, and the second execute
// delivers; both nodes ran across two dag_plan steps.
func TestOrchestrator_IncrementalPlan_SecondExecuteDeclaresDeliveryAndEndsTurn(t *testing.T) {
	stub := &incrementalStub{orchStub: orchStub{replies: []*model.LLMResponse{partialPlanCall()}}}
	o := newTestOrch(t, stub)

	evs := runTurn(t, o, "research the thing, then write it up")

	if hasEvent(evs, stream.EventError) {
		t.Fatalf("run surfaced an error; events=%v", evs)
	}
	if got := countEvent(evs, stream.EventDagPlan); got < 2 {
		t.Errorf("dag_plan events = %d, want at least 2 (one per execute step)", got)
	}
	if got := countEvent(evs, stream.EventNodeDone); got != 2 {
		t.Errorf("node_done events = %d, want 2 (A then B, across two execute steps)", got)
	}
	answer := o.LatestAnswer(context.Background(), "u", "chat")
	if !strings.Contains(answer, "RESEARCH-RESULT") {
		t.Errorf("plan answer = %q, want the terminal node's output", answer)
	}
}
