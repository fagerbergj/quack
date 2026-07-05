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

	replay, l, _, done := h.Subscribe("c")
	if !done || l != nil {
		t.Errorf("late join: done=%v live=%v, want done + nil live", done, l)
	}
	if len(replay) != 2 {
		t.Errorf("late replay = %d events, want 2 (a + done)", len(replay))
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
