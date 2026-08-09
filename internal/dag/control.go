package dag

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// queuedMsg: one message in a running node's queue. Delivered messages are kept but immutable.
type queuedMsg struct {
	ID        string
	Text      string
	Delivered bool
	CreatedAt time.Time
}

func newMsgID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// nodeControl implements vetting.NodeControl: cooperative cancel/pause/queue at gate stage boundaries.
type nodeControl struct {
	mu        sync.Mutex
	cancelled bool
	paused    bool
	queue     []*queuedMsg
	drained   [][]string // one entry per TakeQueued() drain, in order - generation N (the -sN run suffix) reads drained[N-1]
}

func (c *nodeControl) Cancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled
}

func (c *nodeControl) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// TakeQueued drains pending messages into one guidance block; only delivery path at gate boundaries.
func (c *nodeControl) TakeQueued() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.queue {
		if !m.Delivered {
			m.Delivered = true
			out = append(out, m.Text)
		}
	}
	if len(out) == 0 {
		return ""
	}
	c.drained = append(c.drained, out)
	return strings.Join(out, "\n\n")
}

// drainedGeneration returns the Nth drain's guidance text (1-based).
func (c *nodeControl) drainedGeneration(n int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 1 || n > len(c.drained) {
		return ""
	}
	return strings.Join(c.drained[n-1], "\n\n")
}

func (c *nodeControl) markCancelled() {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()
}

func (c *nodeControl) markPaused(v bool) {
	c.mu.Lock()
	c.paused = v
	c.mu.Unlock()
}

// enqueue appends a new queued message and returns a copy.
func (c *nodeControl) enqueue(text string) queuedMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := &queuedMsg{ID: newMsgID(), Text: text, CreatedAt: time.Now().UTC()}
	c.queue = append(c.queue, m)
	return *m
}

// editQueued rewrites a not-yet-delivered message's text.
func (c *nodeControl) editQueued(id, text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.queue {
		if m.ID == id {
			if m.Delivered {
				return false
			}
			m.Text = text
			return true
		}
	}
	return false
}

// removeQueued drops a not-yet-delivered message.
func (c *nodeControl) removeQueued(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, m := range c.queue {
		if m.ID == id {
			if m.Delivered {
				return false
			}
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return true
		}
	}
	return false
}

// snapshotQueue returns a copy of the current queue for SSE sync.
func (c *nodeControl) snapshotQueue() []queuedMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]queuedMsg, len(c.queue))
	for i, m := range c.queue {
		out[i] = *m
	}
	return out
}

// runControls tracks per-chat, per-node controls for active runs.
// ponytail: plain mutex-guarded map.
type runControls struct {
	mu        sync.Mutex
	m         map[string]map[string]*nodeControl // chatID → nodeID → control (live)
	cancelled map[string]map[string]bool         // chatID → nodeID → user-cancelled; persists after the control is unregistered so the stream can mark the node "cancelled" (not "failed")
	paused    map[string]map[string]bool         // chatID → nodeID → user-paused this run; persists past unregister, same reason as cancelled
	overrides map[string]map[string]string       // chatID → nodeID → pending prompt edit for a not-yet-started node (see graph.go's effectiveNode.Task)
}

func newRunControls() *runControls {
	return &runControls{
		m:         map[string]map[string]*nodeControl{},
		cancelled: map[string]map[string]bool{},
		paused:    map[string]map[string]bool{},
		overrides: map[string]map[string]string{},
	}
}

// wasCancelled reports whether a node was user-cancelled (survives unregister).
func (r *runControls) wasCancelled(chatID, nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled[chatID][nodeID]
}

// wasPaused reports whether a node was user-paused (survives unregister).
func (r *runControls) wasPaused(chatID, nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[chatID][nodeID]
}

// resetCancelled clears flags and overrides for a new turn.
func (r *runControls) resetCancelled(chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancelled, chatID)
	delete(r.paused, chatID)
	delete(r.overrides, chatID)
}

// registerAndTakeOverride registers the node and reads+deletes any pending task override atomically.
func (r *runControls) registerAndTakeOverride(chatID, nodeID string) (c *nodeControl, override string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[chatID] == nil {
		r.m[chatID] = map[string]*nodeControl{}
	}
	c = &nodeControl{}
	r.m[chatID][nodeID] = c
	if m := r.overrides[chatID]; m != nil {
		if t, present := m[nodeID]; present {
			override, ok = t, true
			delete(m, nodeID)
		}
	}
	return c, override, ok
}

func (r *runControls) unregister(chatID, nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.m[chatID]; m != nil {
		delete(m, nodeID)
		if len(m) == 0 {
			delete(r.m, chatID)
		}
	}
}

func (r *runControls) get(chatID, nodeID string) *nodeControl {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.m[chatID]; m != nil {
		return m[nodeID]
	}
	return nil
}

// setOverrideIfNotStarted stashes a pending task edit for a not-yet-started node.
func (r *runControls) setOverrideIfNotStarted(chatID, nodeID, task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.m[chatID]; m != nil && m[nodeID] != nil {
		return false // already running - immutable
	}
	if r.overrides[chatID] == nil {
		r.overrides[chatID] = map[string]string{}
	}
	r.overrides[chatID][nodeID] = task
	return true
}

// CancelNode stops one running node; rest of DAG continues. Returns false if node isn't running.
func (e *Executor) CancelNode(chatID, nodeID string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	c.markCancelled()
	e.controls.mu.Lock()
	if e.controls.cancelled[chatID] == nil {
		e.controls.cancelled[chatID] = map[string]bool{}
	}
	e.controls.cancelled[chatID][nodeID] = true
	e.controls.mu.Unlock()
	return true
}

// NodeCancelled queries cancel state for the tool layer (fast-fails the next tool call).
func (e *Executor) NodeCancelled(chatID, nodeID string) bool {
	return e.controls.wasCancelled(chatID, nodeID)
}

// PauseNode suspends a running node, resumable via retry re-run.
func (e *Executor) PauseNode(chatID, nodeID string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	c.markPaused(true)
	e.controls.mu.Lock()
	if e.controls.paused[chatID] == nil {
		e.controls.paused[chatID] = map[string]bool{}
	}
	e.controls.paused[chatID][nodeID] = true
	e.controls.mu.Unlock()
	return true
}

// QueueNodeMessage appends a message to a running node's queue.
func (e *Executor) QueueNodeMessage(chatID, nodeID, text string) (QueuedMessage, bool) {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return QueuedMessage{}, false
	}
	m := c.enqueue(text)
	return toQueuedMessage(m), true
}

// NodeQueueGuidance returns the Nth queue drain's guidance text for node_steered SSE.
func (e *Executor) NodeQueueGuidance(chatID, nodeID string, gen int) string {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return ""
	}
	return c.drainedGeneration(gen)
}

// EditQueuedMessage rewrites a not-yet-delivered queued message.
func (e *Executor) EditQueuedMessage(chatID, nodeID, messageID, text string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	return c.editQueued(messageID, text)
}

// RemoveQueuedMessage drops a not-yet-delivered queued message.
func (e *Executor) RemoveQueuedMessage(chatID, nodeID, messageID string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	return c.removeQueued(messageID)
}

// NodeQueue returns the current queue for a running node (nil if not live).
func (e *Executor) NodeQueue(chatID, nodeID string) []QueuedMessage {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return nil
	}
	snap := c.snapshotQueue()
	out := make([]QueuedMessage, len(snap))
	for i, m := range snap {
		out[i] = toQueuedMessage(m)
	}
	return out
}

// QueuedMessage is the REST/SSE-facing shape.
type QueuedMessage struct {
	ID        string
	Text      string
	Delivered bool
	CreatedAt time.Time
}

func toQueuedMessage(m queuedMsg) QueuedMessage {
	return QueuedMessage{ID: m.ID, Text: m.Text, Delivered: m.Delivered, CreatedAt: m.CreatedAt}
}

// SetNodeTaskOverride edits a not-yet-started node's task. Atomic against the start race.
func (e *Executor) SetNodeTaskOverride(chatID, nodeID, task string) bool {
	return e.controls.setOverrideIfNotStarted(chatID, nodeID, task)
}
