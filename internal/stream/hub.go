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
	runs        map[string]*runHandle // chatID → run; under mu so RegisterRun can't land inside EndRun's check-and-delete
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
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs[chatID] = &runHandle{responseID: responseID, cancel: cancel}
}

// Drops cancel handle after run ends (idempotent). Blind - see EndRun for the
// guarded version production run-ending paths must use.
func (h *Hub) UnregisterRun(chatID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.runs, chatID)
}

// EndRun retires responseID's run and closes its topic via compare-and-delete: a newer run's handle,
// registered by a fast retry, is left alone.
func (h *Hub) EndRun(chatID, responseID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rh, ok := h.runs[chatID]; ok && rh.responseID != responseID {
		return
	}
	delete(h.runs, chatID)
	h.closeLocked(chatID)
}

// Unconditional cancel (DELETE-chat path, no response ID).
func (h *Hub) CancelRun(chatID string) bool {
	h.mu.Lock()
	rh, ok := h.runs[chatID]
	h.mu.Unlock()
	if !ok {
		return false
	}
	rh.cancel()
	return true
}

// HasRegisteredRun reports a queued or executing run; unlike Active it covers unadmitted runs. Used by workspace GC.
func (h *Hub) HasRegisteredRun(chatID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.runs[chatID]
	return ok
}

// ActiveChatIDs snapshots every chat with a run currently registered - the
// set graceful shutdown needs to drain (internal/serve.DrainActiveRuns).
func (h *Hub) ActiveChatIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.runs))
	for k := range h.runs {
		ids = append(ids, k)
	}
	return ids
}

// BeginDraining marks shutdown: dispatch entrypoints check Draining and refuse runs this process won't finish.
func (h *Hub) BeginDraining() { h.draining.Store(true) }

// Draining reports whether BeginDraining has been called.
func (h *Hub) Draining() bool { return h.draining.Load() }

// MarkInterrupted flags chatID's run as cut short by shutdown, so its tail can tell that from an error.
func (h *Hub) MarkInterrupted(chatID string) { h.interrupted.Store(chatID, struct{}{}) }

// WasInterrupted reports and clears chatID's interrupted flag - read once, at
// the run's own tail.
func (h *Hub) WasInterrupted(chatID string) bool {
	_, ok := h.interrupted.LoadAndDelete(chatID)
	return ok
}

// Cancels chatID's active run only if responseID names it (guards against stale ids).
func (h *Hub) CancelResponse(chatID, responseID string) bool {
	h.mu.Lock()
	rh, ok := h.runs[chatID]
	h.mu.Unlock()
	if !ok || rh.responseID != responseID {
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

// ponytail: topic structs (not buffers - Close frees those) are retained per chat forever; a live run's buffer is bounded by MaxReplay. Fine for a single self-hosted instance. Upgrade path if it grows: LRU/TTL eviction of done topics, or a shared event bus when running multiple replicas.
func NewHub() *Hub { return &Hub{topics: map[string]*topic{}, runs: map[string]*runHandle{}} }

// Publish appends a sequenced event and fans it out; the first publish after done starts a fresh topic.
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
		// Non-blocking: a slow subscriber is DROPPED (closed), not skipped past, so it reconnects and replays
		// from its last contiguous id instead of silently losing a range.
		select {
		case ch <- it:
		default:
			close(ch)
			delete(t.subs, ch)
		}
	}
}

// Active reports a live run. Gated on started, not topic existence: Subscribe auto-vivifies empty topics.
func (h *Hub) Active(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.topics[key]
	return t != nil && t.started && !t.done
}

// Close marks the run finished, closes subscribers, and frees the replay buffer; chat_events serves cold replay.
func (h *Hub) Close(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeLocked(key)
}

// closeLocked is Close for callers holding h.mu (EndRun needs the check and close as one atomic step).
func (h *Hub) closeLocked(key string) {
	t := h.topics[key]
	if t == nil {
		return
	}
	t.done = true
	t.buf = nil
	for ch := range t.subs {
		close(ch)
		delete(t.subs, ch)
	}
}

// Reset drops the chat's topic so a new run gets a fresh buffer, closing live subscribers first so none
// hang on a channel that is never fed nor closed.
func (h *Hub) Reset(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t := h.topics[key]; t != nil {
		for ch := range t.subs {
			close(ch)
			delete(t.subs, ch)
		}
	}
	delete(h.topics, key)
}

// Subscribe returns replay and a live channel atomically; done=true means the run finished and live is nil.
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
