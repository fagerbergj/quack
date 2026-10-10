package serve

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
)

// planningFailureModel always fails with a gateway-shaped error, before any DAG node or plan exists.
type planningFailureModel struct{}

func (planningFailureModel) Name() string { return "planning-failure-stub" }

func (planningFailureModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New(`openai qwen3.8-27b (generate): status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`))
	}
}

// noopExtWithRunObserver is the minimal extsdk.Extension a dispatch needs, plus RunObserver
// so the test can capture the outcome driveExtensionRunEvents actually produces.
type noopExtWithRunObserver struct {
	outcomes chan extsdk.RunOutcome
}

func (noopExtWithRunObserver) Tools() []tool.Tool                             { return nil }
func (noopExtWithRunObserver) RegisterRoutes(_, _ chi.Router)                 {}
func (e noopExtWithRunObserver) RunEnded(_ string, outcome extsdk.RunOutcome) { e.outcomes <- outcome }

// TestPlanningFailure_EndsRunFailedWithClassifiedError: a gateway failure in the planning turn (no DagNode)
// still ends RunFailed with the sanitized error, not mapExtRunOutcome's silent-gap default.
func TestPlanningFailure_EndsRunFailedWithClassifiedError(t *testing.T) {
	failing := inference.TracedModelForTesting(planningFailureModel{}, "test-model")
	st, orch, hub, artifacts, _ := newExtTestStackWithModel(t, failing)

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	ext := &noopExtWithRunObserver{outcomes: make(chan extsdk.RunOutcome, 1)}
	var extHolder atomic.Pointer[extsdk.Extension]
	var asExt extsdk.Extension = ext
	extHolder.Store(&asExt)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	const localID = "planning-failure-1156"
	chatID := "ext:noop:" + localID
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID}, Ask: extsdk.Ask{Message: "do something"}}
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
		t.Fatalf("Status = %q, want %q (a gateway failure during planning must not surface as a silent gap)", outcome.Status, extsdk.RunFailed)
	}
	if outcome.Answer != "" {
		t.Errorf("Answer = %q, want empty - the cause belongs in Error", outcome.Answer)
	}
	if !strings.Contains(outcome.Error, "model gateway returned 502 Bad Gateway") {
		t.Errorf("Error = %q, want it to name the classified gateway error", outcome.Error)
	}
	for _, leaked := range []string{"llm-swap", "11436", "POST"} {
		if strings.Contains(outcome.Error, leaked) {
			t.Errorf("Error = %q leaked %q - raw URL/body must never reach the extension", outcome.Error, leaked)
		}
	}
}
