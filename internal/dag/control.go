package dag

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// queuedMsg is one message in a running node's queue: appended by the user,
// drained (all at once, joined) at the node's next turn boundary. Delivered
// messages are kept (for history) but are immutable — edit/remove only act on
// the not-yet-delivered ones.
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

// nodeControl is the live handle to one running gated node. It implements
// vetting.NodeControl: cancel, pause, and the queue drain are cooperative,
// taking effect at the gate's stage boundaries (not mid-model-call — see docs
// Phase 3c) — EXCEPT cancel, whose tool-layer check (NodeCancelled) makes it
// land within one tool call instead of waiting for a boundary.
type nodeControl struct {
	mu        sync.Mutex
	cancelled bool
	paused    bool
	queue     []*queuedMsg
	drained   [][]string // one entry per TakeQueued() drain, in order — generation N (the -sN run suffix) reads drained[N-1]
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

// TakeQueued drains every not-yet-delivered queued message (in order),
// marking each delivered, and returns them joined into one guidance block
// ("" if the queue had nothing pending). This is the ONLY delivery path —
// unlike the old steer, nothing reaches the node mid-turn; a queued message
// only ever lands here, at a gate-stage boundary.
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
	s := out[0]
	for _, more := range out[1:] {
		s += "\n\n" + more
	}
	return s
}

// drainedGeneration returns the joined guidance text of the Nth drain
// (1-based — the -sN run-ID suffix), or "" if unknown.
func (c *nodeControl) drainedGeneration(n int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 1 || n > len(c.drained) {
		return ""
	}
	out := c.drained[n-1]
	s := out[0]
	for _, more := range out[1:] {
		s += "\n\n" + more
	}
	return s
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

// enqueue appends a new queued message and returns it (a copy, safe to hand
// to the caller without the lock).
func (c *nodeControl) enqueue(text string) queuedMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := &queuedMsg{ID: newMsgID(), Text: text, CreatedAt: time.Now().UTC()}
	c.queue = append(c.queue, m)
	return *m
}

// editQueued rewrites a not-yet-delivered message's text. false (no-op) if
// the id is unknown or already delivered.
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

// removeQueued drops a not-yet-delivered message. false (no-op) if the id is
// unknown or already delivered.
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

// snapshotQueue returns a copy of the current queue (delivered + pending), in
// order — for the node_queue SSE sync event.
func (c *nodeControl) snapshotQueue() []queuedMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]queuedMsg, len(c.queue))
	for i, m := range c.queue {
		out[i] = *m
	}
	return out
}

// runControls tracks per-chat, per-node controls for active runs so the
// orchestrator can cancel, pause, or queue a message for a single running
// node while the DAG runs.
// ponytail: a plain mutex-guarded map — one active run per chat, a few nodes.
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

// wasCancelled reports whether a node was user-cancelled this run (survives the
// control's unregister, unlike get()).
func (r *runControls) wasCancelled(chatID, nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled[chatID][nodeID]
}

// wasPaused reports whether a node was user-paused this run (survives the
// control's unregister, mirroring wasCancelled).
func (r *runControls) wasPaused(chatID, nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[chatID][nodeID]
}

// resetCancelled clears a chat's user-cancelled/paused flags and pending task
// overrides. Called at the start of each new turn so a node ID (n1, n2, …
// reused across plans) can't leak stale control state into the next turn's
// same-ID node.
func (r *runControls) resetCancelled(chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancelled, chatID)
	delete(r.paused, chatID)
	delete(r.overrides, chatID)
}

// registerAndTakeOverride registers a live control for the node and, in the
// SAME critical section, does an atomic, ONE-SHOT read of any pending task
// override for it — so a SetNodeTaskOverride call racing this node's start
// either lands strictly before (the node body sees it) or is rejected
// outright (setOverrideIfNotStarted sees the now-live control), never a
// lost-update in between. The override is consumed (deleted) here, not just
// read: a node invoked again later (HITL resume, a fresh retry
// re-registering the same node ID) must fall back to the plan's own
// node.Task, not keep silently reapplying a one-time edit. ok reports
// whether an override was present.
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

// setOverrideIfNotStarted stashes a not-yet-started node's edited task text,
// read by graph.go's node body (effectiveTask) at registerAndTakeOverride
// instead of the plan's own node.Task. In-memory only — it applies to the
// CURRENT run's already-built graph, not a future turn's fresh plan, which
// would carry the edit moot anyway.
//
// The "not started" check (no live control registered) and the write happen
// under the SAME lock as register/registerAndTakeOverride, closing the race
// where a node starts between a separate check-then-set: either this call
// sees the control already live and rejects (false), or it wins and the
// override is guaranteed visible to the node's own registerAndTakeOverride
// (which cannot have run yet — this call and register share one mutex).
func (r *runControls) setOverrideIfNotStarted(chatID, nodeID, task string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.m[chatID]; m != nil && m[nodeID] != nil {
		return false // already running — immutable
	}
	if r.overrides[chatID] == nil {
		r.overrides[chatID] = map[string]string{}
	}
	r.overrides[chatID][nodeID] = task
	return true
}

// CancelNode stops one running node of a chat's active run at its next gate-stage
// boundary; the rest of the DAG keeps going (continue-but-warn). Returns false if
// no such node is running.
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

// NodeCancelled reports whether a node of a chat's active run was cancelled by
// the user. It is the query side of CancelNode, wired into the TOOL layer
// (tools.Deps.NodeCancelled, via internal/serve): a cancelled worker's very next
// tool call fails fast instead of grinding on until the gate's next stage
// boundary, which is what made cancel look like a no-op.
//
// It reads the same wasCancelled flag the gate reads, and outlives the node's
// control registration on purpose: an in-flight tool call that lands just after
// the node unregisters must still be told to stop.
func (e *Executor) NodeCancelled(chatID, nodeID string) bool {
	return e.controls.wasCancelled(chatID, nodeID)
}

// PauseNode suspends one running node of a chat's active run at its next
// gate-stage boundary, keeping its accumulated answer (see
// vetting.ErrNodePaused). Returns false if no such node is running.
//
// ponytail note on the checkpoint: ADK v2's workflow graph is static, so the
// gate-node's own return is what unblocks its dependents — there is no way to
// literally freeze the graph mid-flight the way an ask_user HITL pause does.
// Pause therefore behaves like cancel (the node's current attempt ends,
// dependents proceed continue-but-warn on its partial answer) but is
// RESUMABLE: resuming re-runs the node as a fresh retry (paused → running,
// see the REST handler), reusing the rest of the plan's stored outputs. It
// is not a frozen mid-tool-call checkpoint; a true one would need the same
// ResumeOrRequestInput machinery ask_user uses, keyed by a user-triggered
// (not worker-raised) interrupt — a larger lift left for a follow-up.
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

// QueueNodeMessage appends a message to a running node's queue, drained at
// its next turn boundary (never mid-turn). Returns the created message and
// false if no such node is running.
func (e *Executor) QueueNodeMessage(chatID, nodeID, text string) (QueuedMessage, bool) {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return QueuedMessage{}, false
	}
	m := c.enqueue(text)
	return toQueuedMessage(m), true
}

// NodeQueueGuidance returns the joined guidance text of a node's Nth queue
// drain (1-based — the -sN run-ID suffix) for the node_steered SSE event, or
// "" if unknown or the node isn't live.
func (e *Executor) NodeQueueGuidance(chatID, nodeID string, gen int) string {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return ""
	}
	return c.drainedGeneration(gen)
}

// EditQueuedMessage rewrites a not-yet-delivered queued message. false if no
// such node/message, or the message was already delivered.
func (e *Executor) EditQueuedMessage(chatID, nodeID, messageID, text string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	return c.editQueued(messageID, text)
}

// RemoveQueuedMessage drops a not-yet-delivered queued message. false if no
// such node/message, or the message was already delivered.
func (e *Executor) RemoveQueuedMessage(chatID, nodeID, messageID string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	return c.removeQueued(messageID)
}

// NodeQueue returns the current queue (delivered + pending) for a running
// node, for the queue-mutation endpoints' response and the node_queue SSE
// sync event. nil if the node isn't live.
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

// QueuedMessage is the exported (REST/SSE-facing) shape of a queued message.
type QueuedMessage struct {
	ID        string
	Text      string
	Delivered bool
	CreatedAt time.Time
}

func toQueuedMessage(m queuedMsg) QueuedMessage {
	return QueuedMessage{ID: m.ID, Text: m.Text, Delivered: m.Delivered, CreatedAt: m.CreatedAt}
}

// SetNodeTaskOverride edits a NOT-YET-STARTED node's task text. Returns false
// if the node has already started (its control is registered) — its prompt
// is then immutable. Atomic against the start race (see
// runControls.setOverrideIfNotStarted): a node beginning at the exact instant
// of this call either loses the race outright (this returns false) or wins it
// cleanly (the node is guaranteed to see the override) — never a lost update.
func (e *Executor) SetNodeTaskOverride(chatID, nodeID, task string) bool {
	return e.controls.setOverrideIfNotStarted(chatID, nodeID, task)
}
