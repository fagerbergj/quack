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

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
)

// guardLoopModel repeats an identical malformed create_plan call forever, ignoring the repeat guard's
// REFUSED error.
type guardLoopModel struct{}

func (guardLoopModel) Name() string { return "guard-loop-stub" }

func (guardLoopModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: "create_plan", Args: map[string]any{
					"assignments": []any{map[string]any{"task": "x"}},
				}},
			}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// TestGuardHardStop_EndsRunFailedWithLoopReason: a guard hard-stop stamps RunStatusFailed naming the
// repeated tool (via the plan-rejection give-up path), not RunStatusIdle/RunDone.
func TestGuardHardStop_EndsRunFailedWithLoopReason(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStackWithModel(t, guardLoopModel{})

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	ext := &noopExtWithRunObserver{outcomes: make(chan extsdk.RunOutcome, 1)}
	var extHolder atomic.Pointer[extsdk.Extension]
	var asExt extsdk.Extension = ext
	extHolder.Store(&asExt)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	const localID = "guard-hard-stop-1391"
	chatID := "ext:noop:" + localID
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "do the flaky retry loop review"}}
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
		t.Fatalf("Status = %q, want %q (a guard hard-stop must not surface as done)", outcome.Status, extsdk.RunFailed)
	}
	if outcome.Answer != "" {
		t.Errorf("Answer = %q, want empty - the call never stopped being malformed", outcome.Answer)
	}
	if !strings.Contains(outcome.Error, "create_plan") {
		t.Errorf("Error = %q, want it to name the tool that kept repeating", outcome.Error)
	}

	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat: %v, %v", c, err)
	}
	if c.RunStatus != "failed" {
		t.Errorf("Chat.RunStatus = %q, want failed", c.RunStatus)
	}
}
