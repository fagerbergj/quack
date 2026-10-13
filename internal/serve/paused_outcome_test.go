package serve

import (
	"context"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/stream"
)

// pauseOnceLLM blocks the first worker round until its context is cut, then answers like resumeStubLLM.
type pauseOnceLLM struct {
	*resumeStubLLM
	calls   atomic.Int32
	started chan struct{}
}

func (s *pauseOnceLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, isStream bool) iter.Seq2[*model.LLMResponse, error] {
	if req.Config != nil && len(req.Config.Tools) > 0 || s.calls.Add(1) > 1 {
		return s.resumeStubLLM.GenerateContent(ctx, req, isStream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		s.started <- struct{}{}
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// syncOutcomes records every RunEnded; REST delivers it from the run's goroutine.
type syncOutcomes struct {
	fakeRunObserver
	mu   sync.Mutex
	outs []extsdk.RunOutcome
}

func (o *syncOutcomes) RunEnded(_ string, out extsdk.RunOutcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outs = append(o.outs, out)
}

func (o *syncOutcomes) get() []extsdk.RunOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]extsdk.RunOutcome(nil), o.outs...)
}

// A user pause parks the extension's run as RunPaused (not done with no answer), and resuming the node from
// the REST API tells the extension how the resumed run ended.
func TestUserPausedExtRunEndsPausedAndRESTResumeEndsIt(t *testing.T) {
	ctx := context.Background()
	const chatID = "ext:obs:job-1"
	stub := &pauseOnceLLM{resumeStubLLM: &resumeStubLLM{}, started: make(chan struct{}, 1)}
	st, orch, plan := newOneNodeFixtureWithModel(t, chatID, stub)
	obs := &syncOutcomes{}
	var ext extsdk.Extension = obs
	var holder atomic.Pointer[extsdk.Extension]
	holder.Store(&ext)
	hub := stream.NewHub()

	go func() {
		<-stub.started
		orch.PauseNode(chatID, "n1", dag.PauseUser)
	}()
	userID := st.SessionUserForChat(ctx, chatID)
	driveExtensionRunEvents(ctx, "obs", orch, st, hub, runlog.NewEventLog(st), &holder, userID, chatID, "turn-1", 0,
		func(runCtx context.Context) iter.Seq2[stream.SSEEvent, error] {
			return orch.RetryNode(runCtx, userID, chatID, plan.ID, nil, "n1", "")
		})
	if got := obs.get(); len(got) != 1 || got[0].Status != extsdk.RunPaused || got[0].NodeID != "n1" {
		t.Fatalf("RunEnded after the pause = %+v, want one RunPaused naming n1", got)
	}

	h := rest.NewHandler(st, orch, nil, nil, hub, nil, "test", nil, nil, nil, nil)
	h.SetRunEnded(restRunEnded(orch, st, []builtSDKExtension{{name: "obs", ext: obs}}))
	rec := httptest.NewRecorder()
	h.StartNode(rec, httptest.NewRequest(http.MethodPost, "/", nil), chatID, "n1")
	if rec.Code != http.StatusOK {
		t.Fatalf("StartNode = %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(obs.get()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := obs.get(); len(got) != 2 || got[1].Status != extsdk.RunDone || !got[1].PlanRan {
		t.Fatalf("RunEnded after the resume = %+v, want a second outcome, RunDone with its plan", got)
	}
}

// pausedNodeID counts a pause, not a node parked on a question (RunNeedsInput covers that).
func TestPausedNodeIDIgnoresQuestions(t *testing.T) {
	ctx := context.Background()
	st, _, plan := newOneNodeResumeFixture(t, "chat-paused-id")
	if got := pausedNodeID(ctx, st, plan.ID); got != "n1" {
		t.Errorf("pausedNodeID = %q, want n1 for a paused node", got)
	}
	if got := pausedNodeID(ctx, st, ""); got != "" {
		t.Errorf("pausedNodeID with no plan = %q, want none", got)
	}
	if err := st.SetNodeStatus(ctx, plan.ID, "n1", "", dag.PauseAwaitingInput, "which?"); err != nil {
		t.Fatal(err)
	}
	if got := pausedNodeID(ctx, st, plan.ID); got != "" {
		t.Errorf("pausedNodeID = %q, want none for a node awaiting an answer", got)
	}
}
