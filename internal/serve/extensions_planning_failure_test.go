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
)

// planningFailureModel always fails GenerateContent with a gateway-shaped
// error - #1156's repro: the model endpoint is unreachable during planning,
// before any DAG node/plan exists to attach a DagNode.Error to.
type planningFailureModel struct{}

func (planningFailureModel) Name() string { return "planning-failure-stub" }

func (planningFailureModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New(`openai qwen3.8-27b (generate): status 502: POST "http://llm-swap:11436/v1/chat/completions": 502 Bad Gateway`))
	}
}

// noopExtWithRunObserver is the minimal extsdk.Extension a dispatch needs to
// register, plus RunObserver so the test can capture the outcome
// driveExtensionRunEvents actually produces (mirrors how a real extension -
// github, remarkable - learns a run's terminal status).
type noopExtWithRunObserver struct {
	outcomes chan extsdk.RunOutcome
}

func (noopExtWithRunObserver) Tools() []tool.Tool                              { return nil }
func (noopExtWithRunObserver) RegisterRoutes(_, _ chi.Router)                  {}
func (e noopExtWithRunObserver) RunEnded(_ string, outcome extsdk.RunOutcome) { e.outcomes <- outcome }

// TestPlanningFailure_EndsRunFailedWithClassifiedError is #1156's regression
// guard: a model-gateway failure during the orchestrator's own planning turn
// (no DagNode ever created) must still end the run RunFailed with the
// sanitized gateway error - not fall through to the generic silent-gap text
// mapExtRunOutcome's default branch produces for status=idle/done. Extends
// #1109's own gateway-classification coverage (dag/executor_test.go,
// extensions_cancel_test.go) to the pre-DAG planning path #1109 missed.
func TestPlanningFailure_EndsRunFailedWithClassifiedError(t *testing.T) {
	failing := inference.TracedModelForTesting(planningFailureModel{}, "test-model")
	st, orch, hub, artifacts, _ := newExtTestStackWithModel(t, failing)

	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	ext := &noopExtWithRunObserver{outcomes: make(chan extsdk.RunOutcome, 1)}
	var extHolder atomic.Pointer[extsdk.Extension]
	var asExt extsdk.Extension = ext
	extHolder.Store(&asExt)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

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
	if !strings.Contains(outcome.Answer, "model gateway returned 502 Bad Gateway") {
		t.Errorf("Answer = %q, want it to name the classified gateway error", outcome.Answer)
	}
	for _, leaked := range []string{"llm-swap", "11436", "POST"} {
		if strings.Contains(outcome.Answer, leaked) {
			t.Errorf("Answer = %q leaked %q - raw URL/body must never reach the extension", outcome.Answer, leaked)
		}
	}
}
