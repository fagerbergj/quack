package stream

import (
	"testing"
	"time"
)

func ev(name string) SSEEvent { return SSEEvent{Name: name} }

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live event")
		return Event{}
	}
}

// Events published before a subscriber joins are replayed (with their seq);
// subsequent events reach every live subscriber.
func TestHubReplayAndFanout(t *testing.T) {
	h := NewHub()
	h.Publish("c", 1, ev("a")) // before anyone subscribes

	replay, l1, c1, done := h.Subscribe("c")
	defer c1()
	if done {
		t.Fatal("topic should be active")
	}
	if len(replay) != 1 || replay[0].SSE.Name != "a" || replay[0].Seq != 1 {
		t.Fatalf("replay = %v, want [{1 a}]", replay)
	}
	_, l2, c2, _ := h.Subscribe("c")
	defer c2()

	h.Publish("c", 2, ev("b"))
	if e := recv(t, l1); e.SSE.Name != "b" || e.Seq != 2 {
		t.Errorf("sub1 live = %+v, want {2 b}", e)
	}
	if e := recv(t, l2); e.SSE.Name != "b" {
		t.Errorf("sub2 live = %q, want b", e.SSE.Name)
	}
}

// Close delivers any buffered events then closes live channels; a late joiner
// gets the whole run as replay with done=true.
func TestHubClose(t *testing.T) {
	h := NewHub()
	h.Publish("c", 1, ev("a"))
	_, live, cancel, _ := h.Subscribe("c")
	defer cancel()
	h.Publish("c", 2, Done())
	h.Close("c")

	var names []string
	for e := range live { // ranges until Close closed the channel
		names = append(names, e.SSE.Name)
	}
	if len(names) == 0 {
		t.Error("expected the buffered Done before close")
	}

	// Close frees the replay buffer (perf-audit item 4); a finished chat's
	// replay comes from the durable chat_events table, not the hub.
	replay, l, _, done := h.Subscribe("c")
	if !done || l != nil {
		t.Errorf("late join: done=%v live=%v, want done + nil live", done, l)
	}
	if len(replay) != 0 {
		t.Errorf("late replay = %d events, want 0 (buffer freed on Close)", len(replay))
	}
}

// Active reports true for an unpublished-yet topic only after Close, and false
// again once a new run starts (mirrors the running/idle status flip).
func TestHubActive(t *testing.T) {
	h := NewHub()
	if h.Active("c") {
		t.Error("no topic yet: expected inactive")
	}
	h.Publish("c", 1, ev("a"))
	if !h.Active("c") {
		t.Error("published, not closed: expected active")
	}
	h.Close("c")
	if h.Active("c") {
		t.Error("closed: expected inactive")
	}
	h.Publish("c", 1, ev("new"))
	if !h.Active("c") {
		t.Error("new run published: expected active")
	}
}

// A bare Subscribe (no run has ever published) auto-vivifies an empty topic
// so a same-moment Publish never races past the registered subscriber - but
// that placeholder must not itself read as Active, or a chat nobody ever ran
// would show "running" forever (and the REST /stream handler's cold/warm
// split would misfire on every later reconnect to the same never-run chat).
func TestHubActiveNotFooledByBareSubscribe(t *testing.T) {
	h := NewHub()
	_, _, cancel, done := h.Subscribe("c")
	defer cancel()
	if done {
		t.Fatal("a fresh topic has no run yet, so it's not done - subscriber awaits a live tail")
	}
	if h.Active("c") {
		t.Error("Subscribe alone must not make an unrun chat read as Active")
	}
	h.Publish("c", 1, ev("a"))
	if !h.Active("c") {
		t.Error("a real publish on the same (already-subscribed) topic must read as Active")
	}
}

// The first publish after a run ends starts a fresh stream.
func TestHubNewRunResets(t *testing.T) {
	h := NewHub()
	h.Publish("c", 1, ev("old"))
	h.Close("c")
	h.Publish("c", 1, ev("new")) // new run, seq restarts

	replay, _, cancel, done := h.Subscribe("c")
	defer cancel()
	if done {
		t.Error("should be active after the new run's publish")
	}
	if len(replay) != 1 || replay[0].SSE.Name != "new" {
		t.Errorf("reset replay = %v, want [new]", replay)
	}
}

// --- run-cancel registry (#468) ----------------------------------------------
//
// This is the seam that makes DELETE/stop reach a run regardless of which
// driver started it: the REST handler and the GitHub webhook extension both
// register their run's cancel func here (RegisterRun) instead of keeping
// separate, mutually-unreachable maps, so CancelRun/CancelResponse cancel
// either kind of run identically.

func TestHubCancelRun(t *testing.T) {
	h := NewHub()
	cancelled := false
	h.RegisterRun("c1", "r1", func() { cancelled = true })

	if !h.CancelRun("c1") {
		t.Fatal("CancelRun on a registered run should report true")
	}
	if !cancelled {
		t.Error("CancelRun did not invoke the registered cancel func")
	}
}

// CancelRun on a chat with nothing registered - unknown or already-finished -
// must be a safe no-op, not a panic or a false "cancelled".
func TestHubCancelRun_UnknownChatNoOp(t *testing.T) {
	h := NewHub()
	if h.CancelRun("no-such-chat") {
		t.Error("CancelRun on an unregistered chat should report false")
	}
}

func TestHubCancelResponse_MatchesID(t *testing.T) {
	h := NewHub()
	cancelled := false
	h.RegisterRun("c1", "r1", func() { cancelled = true })

	if !h.CancelResponse("c1", "r1") {
		t.Fatal("CancelResponse with the matching response id should report true")
	}
	if !cancelled {
		t.Error("CancelResponse did not invoke the registered cancel func")
	}
}

// A stale response id (from a superseded run) must not cancel whatever is
// running now - mirrors the UI stop button's guard.
func TestHubCancelResponse_WrongIDNoOp(t *testing.T) {
	h := NewHub()
	cancelled := false
	h.RegisterRun("c1", "the-real-one", func() { cancelled = true })

	if h.CancelResponse("c1", "stale") {
		t.Error("CancelResponse with a mismatched response id should report false")
	}
	if cancelled {
		t.Error("CancelResponse must not invoke cancel for a mismatched response id")
	}
}

// UnregisterRun makes a run uncancellable again - the driver calls this once
// its run ends, so a late DELETE/stop on a finished run is a no-op rather than
// reaching into a stale (possibly reused) cancel func.
func TestHubUnregisterRun(t *testing.T) {
	h := NewHub()
	cancelled := false
	h.RegisterRun("c1", "r1", func() { cancelled = true })
	h.UnregisterRun("c1")

	if h.CancelRun("c1") {
		t.Error("CancelRun should report false once the run is unregistered")
	}
	if cancelled {
		t.Error("cancel func must not fire after UnregisterRun")
	}
}

// TestHubEndRun_StaleResponseIDDoesNotWipeNewerRun pins the #1342 review
// finding: if a new turn registers its own run handle for a chat after an
// old run's tail already called cancelRun() but before that old run's
// EndRun executes, the stale EndRun must not delete the NEW run's handle or
// close its topic - CancelResponse/DrainActiveRuns/the new stream all still
// need it live.
func TestHubEndRun_StaleResponseIDDoesNotWipeNewerRun(t *testing.T) {
	h := NewHub()
	h.RegisterRun("c1", "old-run", func() {})
	h.Publish("c1", 1, SSEEvent{})
	_, _, cancelSub, _ := h.Subscribe("c1")
	defer cancelSub()

	h.RegisterRun("c1", "new-run", func() {}) // a fast retry supersedes the old run

	h.EndRun("c1", "old-run") // the old run's own tail, arriving late

	if !h.CancelRun("c1") {
		t.Error("EndRun(old-run) wiped the new run's cancel handle")
	}
	if !h.Active("c1") {
		t.Error("EndRun(old-run) closed the new run's topic")
	}
}

// A GitHub-dispatched run and a REST-started run are both just callers of
// RegisterRun on the same Hub instance - this pins that the registry is
// driver-agnostic: whichever goroutine registered a chat's cancel func, the
// same CancelRun call reaches it. (internal/github.dispatch and
// rest.Handler.startRun both call exactly this method on the shared hub.)
func TestHubCancelRun_ReachesEitherDriver(t *testing.T) {
	h := NewHub()
	uiCancelled, githubCancelled := false, false
	h.RegisterRun("ui-chat", "r1", func() { uiCancelled = true })
	h.RegisterRun("github-acme-widget-1", "r2", func() { githubCancelled = true })

	if !h.CancelRun("ui-chat") || !uiCancelled {
		t.Error("CancelRun must reach a REST-registered run")
	}
	if !h.CancelRun("github-acme-widget-1") || !githubCancelled {
		t.Error("CancelRun must reach a GitHub-dispatched run through the same registry")
	}
}

// --- slow subscriber (finding 6) --------------------------------------------

// A subscriber whose channel backs up past its buffer must be dropped
// (channel closed), not silently skipped mid-stream. Skipping loses a
// contiguous range with no signal to the client, and the resume cursor
// (last id seen) can never recover it. Dropping ends the connection so the
// client sees the error and reconnects, replaying the gap off the buffer
// (or the durable log) from its last contiguous id.
func TestHubPublishDropsSlowSubscriberInsteadOfSkipping(t *testing.T) {
	h := NewHub()
	_, live, cancel, _ := h.Subscribe("c")
	defer cancel()

	// Fill the subscriber's buffered channel (cap 1024) without draining it.
	for i := int64(1); i <= 1024; i++ {
		h.Publish("c", i, ev("a"))
	}
	// The channel is now full: this publish must drop the subscriber
	// (close its channel) instead of silently skipping event 1025.
	h.Publish("c", 1025, ev("b"))

	var drained int
	for range live { // ranges until the hub closes it
		drained++
	}
	if drained != 1024 {
		t.Fatalf("drained %d buffered events before close, want exactly 1024 (event 1025 must not have been delivered)", drained)
	}

	// The dropped subscriber lost nothing durably - only its live channel
	// died. A fresh Subscribe replays the whole run, gap included, from the
	// hub's own buffer (MaxReplay is 10000, well past 1025).
	replay, _, cancel2, _ := h.Subscribe("c")
	defer cancel2()
	if len(replay) != 1025 {
		t.Fatalf("fresh subscribe replay = %d events, want 1025", len(replay))
	}
}
