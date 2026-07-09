package stream

import "sync"

// Hub fans out a chat's SSE event stream to multiple subscribers, so a run
// started on one device can be watched live from another. Events are keyed by
// chat ID (one active run per chat). Each topic keeps a bounded replay buffer so
// a late joiner — or a reconnecting client — gets the events so far, then the
// live tail. In-memory and single-process: fine for the self-hosted instance;
// behind multiple replicas this would need a shared bus (see configuration).
//
// The hub is the live fan-out and the warm-path replay; durability across a
// restart is the DB-backed event log (store.ChatEvent), which the subscribe
// handler falls back to when a topic is cold. Each event carries the per-chat Seq
// the run loop assigned, so the live tail and the durable log share one numbering
// (a reconnecting client resumes by Last-Event-ID).
type Hub struct {
	mu     sync.Mutex
	topics map[string]*topic
}

// MaxReplay caps a topic's replay buffer (events), and the durable log windows to
// the same ceiling. A run that exceeds it loses its oldest events from replay (the
// live tail is unaffected). Generous: real runs are far smaller.
const MaxReplay = 10000

// Event is a sequenced SSE event: Seq is the per-chat monotonic position assigned
// by the run loop, carried so a reconnecting subscriber resumes by Last-Event-ID
// and so the durable log and the live tail number events identically.
type Event struct {
	Seq int64
	SSE SSEEvent
}

type topic struct {
	buf  []Event
	subs map[chan Event]struct{}
	done bool
}

// NewHub returns an empty hub.
//
// ponytail: topics are retained per chat (one bounded buffer each) so a
// completed run can still be replayed; total memory is chats × MaxReplay. Fine
// for a single self-hosted instance. Upgrade path if it grows: LRU/TTL eviction
// of done topics, or a shared event bus when running multiple replicas.
func NewHub() *Hub { return &Hub{topics: map[string]*topic{}} }

// Publish appends a sequenced event to the chat's topic and fans it to live
// subscribers. The first publish after a topic is done (or absent) starts a fresh
// run, discarding the previous run's buffer — so a new turn replaces the old
// stream. seq is the run loop's per-chat position for this event.
func (h *Hub) Publish(key string, seq int64, ev SSEEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	if t == nil || t.done {
		t = &topic{subs: map[chan Event]struct{}{}}
		h.topics[key] = t
	}
	it := Event{Seq: seq, SSE: ev}
	t.buf = append(t.buf, it)
	if len(t.buf) > MaxReplay {
		t.buf = t.buf[len(t.buf)-MaxReplay:]
	}
	for ch := range t.subs {
		// Non-blocking: a subscriber too slow to keep up drops live events; it can
		// reconnect and replay the buffer (or the durable log) to catch up.
		select {
		case ch <- it:
		default:
		}
	}
}

// Active reports whether a chat currently has a live (not yet Closed) run —
// backs the REST status handler's `running` chat status.
func (h *Hub) Active(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	return t != nil && !t.done
}

// Close marks the chat's run finished and closes its live subscriber channels
// (subscribers see the channel close after draining). The replay buffer is kept
// so a device opening the stream after completion still replays it, until the
// next run on this chat resets the topic.
func (h *Hub) Close(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	if t == nil {
		return
	}
	t.done = true
	for ch := range t.subs {
		close(ch)
		delete(t.subs, ch)
	}
}

// Subscribe returns the events so far (replay) plus, when the run is still
// active, a live channel for subsequent events and a cancel func to unsubscribe.
// When done is true the run has finished: replay holds the whole stream and live
// is nil. Snapshot + registration happen under one lock, so no event is missed
// between them.
func (h *Hub) Subscribe(key string) (replay []Event, live <-chan Event, cancel func(), done bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	if t == nil {
		t = &topic{subs: map[chan Event]struct{}{}}
		h.topics[key] = t
	}
	replay = append([]Event(nil), t.buf...)
	if t.done {
		return replay, nil, func() {}, true
	}
	ch := make(chan Event, 1024)
	t.subs[ch] = struct{}{}
	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
	}
	return replay, ch, cancel, false
}
