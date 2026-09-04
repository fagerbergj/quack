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
