package stream

import (
	"context"
	"sync"
	"sync/atomic"
)

// In-memory fan-out of SSE events per chat ID. Also carries the cancel-run registry.
type Hub struct {
	mu          sync.Mutex
	topics      map[string]*topic
	runs        sync.Map // chatID → *runHandle
	draining    atomic.Bool
	interrupted sync.Map // chatID → struct{}, set right before a shutdown force-cancel (see MarkInterrupted)
}

// Cancel handle for a chat's in-flight run (responseID guards against cancelling stale runs).
type runHandle struct {
	responseID string
	cancel     context.CancelFunc
}

// Records active run before goroutine starts (overwrites stale handles).
func (h *Hub) RegisterRun(chatID, responseID string, cancel context.CancelFunc) {
	h.runs.Store(chatID, &runHandle{responseID: responseID, cancel: cancel})
}

// Drops cancel handle after run ends (idempotent).
func (h *Hub) UnregisterRun(chatID string) {
	h.runs.Delete(chatID)
}

// Unconditional cancel (DELETE-chat path, no response ID).
func (h *Hub) CancelRun(chatID string) bool {
	v, ok := h.runs.Load(chatID)
	if !ok {
		return false
	}
	v.(*runHandle).cancel()
	return true
}

// Reports whether chatID has a run registered (queued or executing). Unlike Active, covers runs still waiting to be admitted. Used by workspace GC.
func (h *Hub) HasRegisteredRun(chatID string) bool {
	_, ok := h.runs.Load(chatID)
	return ok
}

// ActiveChatIDs snapshots every chat with a run currently registered - the
// set graceful shutdown needs to drain (internal/serve.DrainActiveRuns).
func (h *Hub) ActiveChatIDs() []string {
	var ids []string
	h.runs.Range(func(k, _ any) bool {
		ids = append(ids, k.(string))
		return true
	})
	return ids
}

// BeginDraining marks the server as shutting down: dispatch entrypoints
// (REST SendChatMessage, SDK extension Dispatch) consult Draining and refuse
// new work, so nothing starts a run this process won't stick around to finish.
func (h *Hub) BeginDraining() { h.draining.Store(true) }

// Draining reports whether BeginDraining has been called.
func (h *Hub) Draining() bool { return h.draining.Load() }

// MarkInterrupted flags chatID's run as cut short by shutdown, set by
// DrainActiveRuns right before its force-cancel so the run's own tail can
// tell that apart from an ordinary error or completion.
func (h *Hub) MarkInterrupted(chatID string) { h.interrupted.Store(chatID, struct{}{}) }

// WasInterrupted reports and clears chatID's interrupted flag - read once, at
// the run's own tail.
func (h *Hub) WasInterrupted(chatID string) bool {
	_, ok := h.interrupted.LoadAndDelete(chatID)
	return ok
}

// Cancels chatID's active run only if responseID names it (guards against stale ids).
func (h *Hub) CancelResponse(chatID, responseID string) bool {
	v, ok := h.runs.Load(chatID)
	if !ok {
		return false
	}
	rh := v.(*runHandle)
	if rh.responseID != responseID {
		return false
	}
	rh.cancel()
	return true
}

// Caps replay buffer (and durable log window). Oldest events are dropped from replay; live tail unaffected.
const MaxReplay = 10000

// Sequenced SSE event: Seq is the per-chat monotonic position, used for Last-Event-ID reconnection.
type Event struct {
	Seq int64
	SSE SSEEvent
}

type topic struct {
	buf     []Event
	subs    map[chan Event]struct{}
	done    bool
	started bool // true once a run has actually Published - see Active.
}

// NewHub returns an empty hub.
//
// ponytail: topics are retained per chat (one bounded buffer each) so a
// completed run can still be replayed; total memory is chats × MaxReplay. Fine
// for a single self-hosted instance. Upgrade path if it grows: LRU/TTL eviction
// of done topics, or a shared event bus when running multiple replicas.
func NewHub() *Hub { return &Hub{topics: map[string]*topic{}} }

// Appends a sequenced event to the chat's topic and fans it to live subscribers. First publish after done starts a fresh topic.
func (h *Hub) Publish(key string, seq int64, ev SSEEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	if t == nil || t.done {
		t = &topic{subs: map[chan Event]struct{}{}}
		h.topics[key] = t
	}
	t.started = true
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

// Reports whether a chat has a live (not yet Closed) run. Gated on started, not topic existence: Subscribe auto-vivifies an empty topic that must not read as "running".
func (h *Hub) Active(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	return t != nil && t.started && !t.done
}

// Marks the run finished and closes live subscriber channels. Replay buffer is kept until the next run resets it.
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

// Drops the chat's topic so a new run gets a fresh buffer. Publish does the same lazily; call this to attach subscribers before publishing.
func (h *Hub) Reset(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.topics, key)
}

// Returns replay events and a live channel. done=true means the run has finished (live is nil). Snapshot + registration are atomic.
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
