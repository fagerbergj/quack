// incremental_plan_test.go: an end-to-end fake-model pin of slice-3's core
// promise - a plan authored as A, then B once A's result is known, run
// across two execute() calls in ONE orchestrator turn: create_plan(A) ->
// execute (partial, no delivery, turn continues) -> edit_plan(add B depends
// on A, declare delivery) -> execute (delivers, turn ends).
package orchestrator

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/stream"
)

// partialPlanCall is the orchestrator authoring a one-assignment plan with NO
// delivery declared - a genuine partial first step, unlike planCall
// (continue_test.go), which declares delivery so most tests' "one execute
// finishes the run" assumption still holds.
func partialPlanCall() *model.LLMResponse {
	return stubCall("create_plan", map[string]any{"assignments": []any{map[string]any{
		"agent": "web-researcher", "task": "research the thing",
	}}})
}

// editPlanAddB builds the edit_plan call that adds assignment B depending on
// aNodeID and declares delivery - the step that turns a partial plan into a
// finished one.
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

// executedPlanIDs collects every plan_id an execute FunctionCall named -
// used here to tell the first (partial) execute call apart from the second
// (delivering) one, since both name the SAME plan_id under incremental planning.
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

// hasFunctionCall reports whether req's history already contains a
// FunctionCall named name - used to make the growth step (edit_plan) fire
// exactly once instead of every invocation, since executedPlanIDs alone
// can't distinguish "edit_plan just ran" from "not yet" (edit_plan never
// itself adds an execute call to that count).
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

// incrementalStub scripts: create_plan(A) -> [auto: first execute, partial]
// -> edit_plan(add B, declare delivery) -> [auto: second execute, delivers].
// It reuses orchStub's auto-execute-once-a-plan_id-is-known behavior for the
// FIRST execute call, then supplies its own scripted edit_plan/execute for
// the growth step, since orchStub's auto-execute would otherwise also fire
// for the second plan_id occurrence.
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
		// The first (partial) execute already ran and edit_plan hasn't fired
		// yet - grow the plan and declare delivery. Gated on edit_plan, not
		// len(execs), since edit_plan itself never adds an execute call - a
		// switch on len(execs) alone would re-issue this every invocation.
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

// TestOrchestrator_IncrementalPlan_SecondExecuteDeclaresDeliveryAndEndsTurn
// pins the whole slice-3 loop in one turn: the first execute's results lead
// to an edit_plan that adds B and declares delivery, and the second execute
// delivers the answer - both nodes actually ran, across two dag_plan steps.
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
