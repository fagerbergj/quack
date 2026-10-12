package serve

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extsdk "github.com/fagerbergj/quack/extensions/sdk"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// assertContiguousEvents fails unless chatID's persisted events run 1..n with none dropped.
func assertContiguousEvents(t *testing.T, st *store.Store, chatID string) {
	t.Helper()
	evs, err := st.LoadChatEvents(context.Background(), chatID, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("LoadChatEvents: %d rows, err %v", len(evs), err)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the log has a gap or a second run's numbering", i, e.Seq)
		}
	}
}

// A re-POSTed job (dogfood finding 2): concurrent dispatches onto one chat start exactly one run, the
// refused ones touch nothing, and the run ends with one RunEnded and an intact event log. Run with -race.
func TestExtDispatch_RefusedWhileTheChatsRunIsLive(t *testing.T) {
	bm := blockingModel{release: make(chan struct{})}
	st, orch, hub, artifacts, _ := newExtTestStackWithModel(t, bm)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	obs := &fakeRunObserver{}
	var ext extsdk.Extension = obs
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&ext)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	const chatID = "ext:noop:6:waivers"
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: "6:waivers"}, Ask: extsdk.Ask{Message: "run waivers"}}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if dispatch(context.Background(), req) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := accepted.Load(); n != 1 {
		t.Fatalf("%d of 4 concurrent dispatches started a run on %s, want exactly 1", n, chatID)
	}

	close(bm.release)
	waitRunEnded(t, hub, chatID)
	if n := obs.ended.Load(); n != 1 {
		t.Errorf("RunEnded fired %d times, want once", n)
	}
	if turns, err := st.ListTurns(context.Background(), chatID); err != nil || len(turns) != 1 {
		t.Errorf("chat has %d turns (err %v), want 1: a refused dispatch must not save its turn", len(turns), err)
	}
	assertContiguousEvents(t, st, chatID)
}

// nudgeObserver re-dispatches onto the chat from inside its first RunEnded, as GitHub's no-plan nudge does.
type nudgeObserver struct {
	fakeRunObserver
	nudge    func() error
	nudgeErr chan error
}

func (o *nudgeObserver) RunEnded(string, extsdk.RunOutcome) {
	if o.ended.Add(1) == 1 {
		o.nudgeErr <- o.nudge()
	}
}

// A dispatch from RunEnded lands while the ended run is still registered; refusing it would drop the nudge.
func TestExtDispatch_FromRunEndedSupersedesTheEndedRun(t *testing.T) {
	st, orch, hub, artifacts, _ := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	obs := &nudgeObserver{nudgeErr: make(chan error, 1)}
	var ext extsdk.Extension = obs
	var extHolder atomic.Pointer[extsdk.Extension]
	extHolder.Store(&ext)
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	const chatID = "ext:noop:nudged"
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: "nudged"}, Ask: extsdk.Ask{Message: "first"}}
	obs.nudge = func() error {
		return dispatch(context.Background(), extsdk.DispatchRequest{Chat: req.Chat, Ask: extsdk.Ask{Message: "nudge"}})
	}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	select {
	case err := <-obs.nudgeErr:
		if err != nil {
			t.Fatalf("nudge dispatched from RunEnded was refused: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first run never reached RunEnded")
	}
	waitUntil(t, 10*time.Second, func() bool { return obs.ended.Load() == 2 })
	waitRunEnded(t, hub, chatID)
	assertContiguousEvents(t, st, chatID)
}

// A dispatch that got the chat while its resume waited must keep it: the resume backs off without
// publishing, delivering RunEnded, or touching the paused node.
func TestDriveResume_LeavesALiveRunAlone(t *testing.T) {
	ctx := context.Background()
	chatID := "ext:obs:busy"
	st, orch, plan := newOneNodeResumeFixture(t, chatID)
	hub := stream.NewHub()
	cancelled := false
	hub.RegisterRun(chatID, "dispatched", func() { cancelled = true })
	obs := &outcomeObserver{}

	driveResume(ctx, chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}},
		orch, st, hub, runlog.NewEventLog(st), obs)

	if len(obs.outcomes) != 0 {
		t.Errorf("RunEnded delivered %+v for a resume that never ran", obs.outcomes)
	}
	if !hub.CancelResponse(chatID, "dispatched") || !cancelled {
		t.Error("the resume replaced the live run's handle")
	}
	if evs, _ := st.LoadChatEvents(ctx, chatID, 0); len(evs) != 0 {
		t.Errorf("the refused resume published %d events", len(evs))
	}
	if n, err := st.GetDagNode(ctx, plan.ID, "n1"); err != nil || n == nil || n.Status != string(dag.StatusPaused) {
		t.Errorf("node n1 = %+v (err %v), want it left paused for the next boot", n, err)
	}
}

// claimObserver records whether a new run could claim the chat from inside RunEnded.
type claimObserver struct {
	fakeRunObserver
	hub     *stream.Hub
	claimed bool
}

func (o *claimObserver) RunEnded(chatID string, _ extsdk.RunOutcome) {
	o.claimed = o.hub.RegisterRun(chatID, "nudge", func() {})
}

// A resumed run's RunEnded may re-dispatch onto its chat, as an uninterrupted run's may.
func TestDriveResume_RunEndedCanClaimTheChat(t *testing.T) {
	chatID := "ext:obs:resumed"
	st, orch, plan := newOneNodeResumeFixture(t, chatID)
	hub := stream.NewHub()
	obs := &claimObserver{hub: hub}

	driveResume(context.Background(), chatID, []store.ResumableNode{{ChatID: chatID, PlanID: plan.ID, NodeID: "n1", Reason: dag.PauseShutdown}},
		orch, st, hub, runlog.NewEventLog(st), obs)

	if !obs.claimed {
		t.Fatal("a dispatch from the resumed run's RunEnded was refused")
	}
	if !hub.CancelResponse(chatID, "nudge") {
		t.Error("the resume's own EndRun dropped the run that superseded it")
	}
}
