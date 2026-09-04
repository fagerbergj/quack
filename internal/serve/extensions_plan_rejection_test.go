package serve

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
)

// rejectedPlanModel always proposes a review-delivery plan that dag's own
// OverrideExistingPRHead deterministically rejects (a review-only setup with
// no real PR head ref in context - #1180's live repro), then ends its
// invocation with no plan and no answer once it sees the rejection, so the
// orchestrator's continuation loop tries again from scratch rather than
// looping forever inside one invocation.
type rejectedPlanModel struct{}

func (rejectedPlanModel) Name() string { return "rejected-plan-stub" }

func (rejectedPlanModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if len(req.Contents) > 0 {
			last := req.Contents[len(req.Contents)-1]
			for _, p := range last.Parts {
				if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == "plan" {
					// Already tried and was rejected this invocation - stop
					// here (empty) so the orchestrator's bounded continuation
					// loop is what re-drives the next attempt, not an
					// unbounded in-invocation retry loop.
					yield(&model.LLMResponse{
						Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}},
						FinishReason: genai.FinishReasonStop,
						TurnComplete: true,
					}, nil)
					return
				}
			}
		}
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: "plan", Args: map[string]any{
					"nodes": []any{map[string]any{
						"id": "review", "agent": "code-reviewer", "task": "review the PR", "depends_on": []any{},
					}},
					"delivery": map[string]any{"kind": "review"},
					"setup":    map[string]any{"repo": "https://github.com/o/r", "base_ref": "main", "work_branch": "quack/pr-1"},
				}},
			}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// TestPlanRejection_EndsRunFailedWithRejectionReason is #1180's regression
// guard: a planner turn whose `plan` calls are all rejected, and that ends
// with no plan and no answer, must end the run failed with the rejection
// text - not the generic "produced no answer - no error" silent-gap comment.
func TestPlanRejection_EndsRunFailedWithRejectionReason(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStackWithModelAndAgents(t, rejectedPlanModel{},
		[]dag.AgentInfo{{Name: "code-reviewer", Description: "reviews PRs"}})

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	ext := &noopExtWithRunObserver{outcomes: make(chan extsdk.RunOutcome, 1)}
	var extHolder atomic.Pointer[extsdk.Extension]
	var asExt extsdk.Extension = ext
	extHolder.Store(&asExt)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	const localID = "plan-rejection-1180"
	chatID := "ext:noop:" + localID
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "review this PR"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	var outcome extsdk.RunOutcome
	select {
	case outcome = <-ext.outcomes:
	case <-time.After(5 * time.Second):
		t.Fatalf("RunEnded was never called")
	}

	if outcome.Status != extsdk.RunFailed {
		t.Fatalf("Status = %q, want %q (a rejected-plan turn with no plan and no answer must not surface as a silent gap)", outcome.Status, extsdk.RunFailed)
	}
	if !strings.Contains(outcome.Answer, "review setup needs the PR's real head branch") {
		t.Errorf("Answer = %q, want it to name the plan's own rejection reason", outcome.Answer)
	}

	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.RunStatus != "failed" {
		t.Errorf("Chat.RunStatus = %q, want failed", c.RunStatus)
	}
}

// rejectThenSilentModel behaves exactly like rejectedPlanModel for its first
// dispatch, then - once the request's own text names the "TURN2" marker -
// always answers empty, never touching the plan tool at all. Simulates a
// SECOND, unrelated turn on the same chat that is a genuine silent gap.
type rejectThenSilentModel struct{}

func (rejectThenSilentModel) Name() string { return "reject-then-silent-stub" }

func (rejectThenSilentModel) GenerateContent(_ context.Context, req *model.LLMRequest, s bool) iter.Seq2[*model.LLMResponse, error] {
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && strings.Contains(p.Text, "TURN2") {
				return func(yield func(*model.LLMResponse, error) bool) {
					yield(&model.LLMResponse{
						Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}},
						FinishReason: genai.FinishReasonStop,
						TurnComplete: true,
					}, nil)
				}
			}
		}
	}
	return rejectedPlanModel{}.GenerateContent(context.Background(), req, s)
}

// TestPlanRejection_DoesNotLeakIntoALaterSilentGap is the #1181 review's
// suggestion: an earlier turn's rejection must not outlive it - a later,
// unrelated turn on the SAME chat that genuinely produces nothing (no plan
// call at all) must derive a true silent gap (idle, no error), not get
// stamped failed with the first turn's stale rejection text.
func TestPlanRejection_DoesNotLeakIntoALaterSilentGap(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStackWithModelAndAgents(t, rejectThenSilentModel{},
		[]dag.AgentInfo{{Name: "code-reviewer", Description: "reviews PRs"}})

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	const localID = "plan-rejection-no-leak-1181"
	chatID := "ext:noop:" + localID
	first := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "review this PR"}}
	if err := dispatch(context.Background(), first); err != nil {
		t.Fatalf("dispatch 1: %v", err)
	}
	waitRunSettled(t, st, chatID)
	if c, _ := st.GetChat(context.Background(), chatID); c == nil || c.RunStatus != "failed" {
		t.Fatalf("turn 1 RunStatus = %+v, want failed (setup for the leak check)", c)
	}

	// waitRunSettled alone would race here: RunStatus is already "failed"
	// (non-empty) from turn 1, so wait for the row's updated_at to move past
	// turn 1's own stamp instead of just "RunStatus is set".
	t1, err := st.GetChat(context.Background(), chatID)
	if err != nil || t1 == nil {
		t.Fatalf("GetChat after turn 1: %v, %v", t1, err)
	}
	turn1UpdatedAt := t1.UpdatedAt
	second := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "TURN2: say nothing useful"}}
	if err := dispatch(context.Background(), second); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	// MarkRunActive alone (turn 2 starting) already bumps updated_at via
	// gorm's auto-timestamp - wait for ActiveTurnID to go non-empty (turn 2
	// truly started) and then clear again (StampTerminalOutcome, turn 2's
	// own finish), not just "updated_at moved".
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := st.GetChat(context.Background(), chatID)
		return c != nil && c.ActiveTurnID != ""
	})
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := st.GetChat(context.Background(), chatID)
		return c != nil && c.ActiveTurnID == "" && c.UpdatedAt.After(turn1UpdatedAt)
	})

	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.RunStatus != "idle" {
		reason, ok := inference.LastPlanRejection(chatID)
		t.Fatalf("turn 2 Chat.RunStatus = %q, want idle - turn 1's rejection must not leak into a later genuine silent gap (LastPlanRejection ok=%v reason=%q)", c.RunStatus, ok, reason)
	}
}
