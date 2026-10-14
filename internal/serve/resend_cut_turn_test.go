package serve

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// cutOnceModel holds its first call until the run is cut, then answers like directAnswerModel.
type cutOnceModel struct {
	calls   atomic.Int32
	entered chan struct{}
}

func (*cutOnceModel) Name() string { return "cut-once-stub" }

func (m *cutOnceModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	if m.calls.Add(1) > 1 {
		return directAnswerModel{}.GenerateContent(ctx, req, stream)
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		close(m.entered)
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

// A chat cut while its orchestrator turn ran, before any node, is sent again at boot and completes.
func TestBootResendsAChatTurnCutBeforeAnyNode(t *testing.T) {
	ctx, chatID := context.Background(), "chat-cut-planning"
	st, orch, hub, _, _ := newExtTestStack(t)
	if err := st.SetChatOrigin(ctx, chatID, "u1", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTurn(ctx, chatID, "turn-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunActive(ctx, chatID, "turn-1"); err != nil {
		t.Fatal(err)
	}

	resume := reconcileNodes(ctx, st, nil, nil)
	if len(resume.Settled) != 0 || len(resume.Resend) != 1 {
		t.Fatalf("Settled = %+v, Resend = %+v; want the cut turn resent, not settled", resume.Settled, resume.Resend)
	}
	eventLog := runlog.NewEventLog(st)
	resendCutTurns(ctx, resume.Resend, orch, st, hub, eventLog, nil)
	waitRunSettled(t, st, chatID)
	waitRunEnded(t, hub, chatID)

	c, err := st.GetChat(ctx, chatID)
	if err != nil || c.RunStatus == store.RunStatusFailed || c.ActiveTurnID != "" {
		t.Fatalf("chat = %+v (%v), want the resent turn ended, not failed", c, err)
	}
	evs, _ := st.LoadChatEvents(ctx, chatID, 0)
	var all strings.Builder
	for _, e := range evs {
		all.WriteString(e.Event)
	}
	if !strings.Contains(all.String(), "quack quack") {
		t.Errorf("transcript lacks the resent turn's answer: %s", all.String())
	}
}

// An extension run cut during its orchestrator turn is dispatched again at boot, and its extension hears
// RunEnded once, for the run that finished.
func TestBootResendsAnExtensionTurnCutBeforeAnyNode(t *testing.T) {
	ctx := context.Background()
	m := &cutOnceModel{entered: make(chan struct{})}
	st, orch, hub, artifacts, _ := newExtTestStackWithModel(t, m)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	ext := &noopExtWithRunObserver{outcomes: make(chan extsdk.RunOutcome, 2)}
	var asExt extsdk.Extension = ext
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&asExt)

	chatID := "ext:noop:cut-1"
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: "cut-1"}, Ask: extsdk.Ask{Message: "review it", NodeContext: "the PR"}}
	if err := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)(ctx, req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	<-m.entered
	hub.MarkInterrupted(chatID) // DrainActiveRuns' force-cancel past the grace
	hub.CancelRun(chatID)
	waitRunEnded(t, hub, chatID)

	// The restart: a fresh hub, and the extension's dispatch bound to it.
	hub2, eventLog2 := stream.NewHub(), runlog.NewEventLog(st)
	resume := reconcileNodes(ctx, st, nil, nil)
	if len(resume.Settled) != 0 || len(resume.Resend) != 1 {
		t.Fatalf("Settled = %+v, Resend = %+v; want the cut turn resent, not settled", resume.Settled, resume.Resend)
	}
	exts := []builtSDKExtension{{name: "noop", ext: ext, dispatch: newExtDispatch("noop", &orchRef, st, hub2, eventLog2, &extHolder, nil, artifacts)}}
	endSettledExtRuns(resume.Settled, exts)
	resendCutTurns(ctx, resume.Resend, orch, st, hub2, eventLog2, exts)

	select {
	case out := <-ext.outcomes:
		if out.Status != extsdk.RunDone {
			t.Fatalf("RunEnded = %+v, want RunDone", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunEnded never came for the resent run")
	}
	waitRunEnded(t, hub2, chatID)
	select {
	case out := <-ext.outcomes:
		t.Errorf("a second RunEnded %+v; the extension must hear the run end once", out)
	case <-time.After(100 * time.Millisecond):
	}
}

// A finished extension turn drops its kept dispatch: only an in-flight turn can be resent.
func TestFinishedExtensionTurnDropsItsDispatch(t *testing.T) {
	ctx, chatID := context.Background(), "ext:noop:done-1"
	st, orch, hub, artifacts, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var asExt extsdk.Extension = &noopExtWithRunObserver{outcomes: make(chan extsdk.RunOutcome, 1)}
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&asExt)
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: "done-1"}, Ask: extsdk.Ask{Message: "review it"}}
	if err := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)(ctx, req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunEnded(t, hub, chatID)

	turns, err := st.ListTurns(ctx, chatID)
	if err != nil || len(turns) != 1 || turns[0].Dispatch != "" {
		t.Fatalf("turns = %+v (%v), want one turn with its dispatch cleared", turns, err)
	}
}
