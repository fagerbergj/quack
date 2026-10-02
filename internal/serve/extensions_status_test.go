package serve

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// blockingModel holds the orchestrator's first LLM call open, like a slow planning turn.
type blockingModel struct {
	directAnswerModel
	release chan struct{}
}

func (m blockingModel) GenerateContent(ctx context.Context, r *model.LLMRequest, s bool) iter.Seq2[*model.LLMResponse, error] {
	select {
	case <-m.release:
	case <-ctx.Done():
	}
	return m.directAnswerModel.GenerateContent(ctx, r, s)
}

// TestExtDispatch_OrchestratorPhaseReadsRunning: an extension-dispatched chat whose previous run
// failed must read running from the moment Dispatch acks, not once the run goroutine gets scheduled.
func TestExtDispatch_OrchestratorPhaseReadsRunning(t *testing.T) {
	// One P keeps the run goroutine unscheduled until the test blocks, so a registration done
	// inside it is reliably missed.
	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
	bm := blockingModel{release: make(chan struct{})}
	st, orch, hub, artifacts, _ := newExtTestStackWithModel(t, bm)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("github", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)
	h := rest.NewHandler(st, orch, nil, nil, hub, nil, "test", nil, nil, artifacts, nil)

	const chatID = "ext:github:github-o-r-1"
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: "github-o-r-1"}, Ask: extsdk.Ask{Message: "review"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !hub.HasRegisteredRun(chatID) {
		t.Fatal("run not registered when Dispatch acked")
	}
	_ = st.StampRunOutcome(context.Background(), chatID, "failed", "") // the PR's earlier run

	status := func() schema.ChatStatus {
		rec := httptest.NewRecorder()
		h.ListChats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil), schema.ListChatsParams{})
		var list schema.ChatList
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, c := range list.Data {
			if c.Id == chatID {
				return c.Status
			}
		}
		t.Fatalf("chat %s not listed", chatID)
		return ""
	}
	if got := status(); got != schema.ChatStatusRunning {
		t.Errorf("status during orchestrator phase = %q, want running", got)
	}
	close(bm.release)
	waitRunSettled(t, st, chatID)
}
