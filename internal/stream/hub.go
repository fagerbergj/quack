package stream

import "sync"

// Hub fans out a chat's SSE event stream to multiple subscribers, so a run
// started on one device can be watched live from another. Events are keyed by
// chat ID (one active run per chat). Each topic keeps a bounded replay buffer so
// a late joiner — or a reconnecting client — gets the events so far, then the
// live tail. In-memory and single-process: fine for the self-hosted instance;
// behind multiple replicas this would need a shared bus (see configuration).
type Hub struct {
	mu     sync.Mutex
	topics map[string]*topic
}

// maxReplay caps a topic's replay buffer (events). A run that exceeds it loses
// its oldest events from replay (the live tail is unaffected). Generous: real
// runs are far smaller.
const maxReplay = 10000

type topic struct {
	buf  []SSEEvent
	subs map[chan SSEEvent]struct{}
	done bool
}

// NewHub returns an empty hub.
//
// ponytail: topics are retained per chat (one bounded buffer each) so a
// completed run can still be replayed; total memory is chats × maxReplay. Fine
// for a single self-hosted instance. Upgrade path if it grows: LRU/TTL eviction
// of done topics, or a shared event bus when running multiple replicas.
func NewHub() *Hub { return &Hub{topics: map[string]*topic{}} }

// Publish appends an event to the chat's topic and fans it to live subscribers.
// The first publish after a topic is done (or absent) starts a fresh run,
// discarding the previous run's buffer — so a new turn replaces the old stream.
func (h *Hub) Publish(key string, ev SSEEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	if t == nil || t.done {
		t = &topic{subs: map[chan SSEEvent]struct{}{}}
		h.topics[key] = t
	}
	t.buf = append(t.buf, ev)
	if len(t.buf) > maxReplay {
		t.buf = t.buf[len(t.buf)-maxReplay:]
	}
	for ch := range t.subs {
		// Non-blocking: a subscriber too slow to keep up drops live events; it can
		// reconnect and replay the buffer to catch up.
		select {
		case ch <- ev:
		default:
		}
	}
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
func (h *Hub) Subscribe(key string) (replay []SSEEvent, live <-chan SSEEvent, cancel func(), done bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	if t == nil {
		t = &topic{subs: map[chan SSEEvent]struct{}{}}
		h.topics[key] = t
	}
	replay = append([]SSEEvent(nil), t.buf...)
	if t.done {
		return replay, nil, func() {}, true
	}
	ch := make(chan SSEEvent, 1024)
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
