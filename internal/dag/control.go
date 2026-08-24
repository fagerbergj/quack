package dag

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// NodeStateStore is the write-through seam for the node state machine: every
// pause/start/stop and every steer-queue edit lands here before it is acted
// on, so a kill can't lose it. Stringly typed on purpose - internal/store
// imports internal/dag, so it cannot take dag types in its signatures.
// Implemented by *store.Store (SetNodeStatusForChat/SetNodeQueue/GetNodeState).
type NodeStateStore interface {
	SetNodeStatusForChat(ctx context.Context, chatID, nodeID, status, pauseReason, pendingQuestion string) error
	SetNodeQueue(ctx context.Context, chatID, nodeID, queueJSON string) error
	GetNodeState(ctx context.Context, chatID, nodeID string) (status, pauseReason, pendingQuestion, queueJSON string, err error)
}

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
	reason    PauseReason
	queue     []*queuedMsg
	drained   [][]string // one entry per TakeQueued() drain, in order - generation N (the -sN run suffix) reads drained[N-1]

	// liveSteer forwards a message into the running round instead of parking
	// it. nil when the node isn't mid-round.
	liveSteer func(text string) bool

	// Write-through coordinates; nil store = in-memory only (tests).
	store          NodeStateStore
	chatID, nodeID string
}

// setLiveSteer/clearLiveSteer: the live round's forward hook, registered for
// the round's duration only - see acp.Agent.round.
func (c *nodeControl) setLiveSteer(f func(text string) bool) {
	c.mu.Lock()
	c.liveSteer = f
	c.mu.Unlock()
}

func (c *nodeControl) clearLiveSteer() {
	c.mu.Lock()
	c.liveSteer = nil
	c.mu.Unlock()
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

// PeekQueued returns pending messages WITHOUT consuming them. Live delivery
// only nudges the running round; the gate boundary still owns durable delivery
// - the prompt fold, the -sN generation record and persistence (#1029).
func (c *nodeControl) PeekQueued() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.queue {
		if !m.Delivered {
			out = append(out, m.Text)
		}
	}
	return strings.Join(out, "\n\n")
}

// TakeQueued drains pending messages into one guidance block; only delivery path at gate boundaries.
func (c *nodeControl) TakeQueued() string {
	c.mu.Lock()
	var out []string
	for _, m := range c.queue {
		if !m.Delivered {
			m.Delivered = true
			out = append(out, m.Text)
		}
	}
	if len(out) == 0 {
		c.mu.Unlock()
		return ""
	}
	c.drained = append(c.drained, out)
	joined := strings.Join(out, "\n\n")
	c.mu.Unlock()
	// Synchronous: an async snapshot could land after a newer enqueue's write
	// and drop a steer from the row (the restart-survival guarantee).
	c.persistQueue()
	return joined
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

// PauseReason reports why the node is paused ("" when it isn't).
func (c *nodeControl) PauseReason() PauseReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.paused {
		return ""
	}
	return c.reason
}

func (c *nodeControl) markCancelled() {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()
	c.persistStatus(StatusCancelled, "", "")
}

// markPaused is the single pause path: HITL, a user pause and shutdown all
// come through here. The store write happens BEFORE the flag is visible, so
// a crash between the two can only lose the in-memory copy, never the row.
func (c *nodeControl) markPaused(reason PauseReason, question string) {
	// Empty status: the HITL park's status arrives on the needs_input event;
	// this write is here for the reason + question.
	status := StatusPaused
	if reason == PauseAwaitingInput {
		status = ""
	}
	c.persistStatus(status, reason, question)
	c.mu.Lock()
	c.paused, c.reason = true, reason
	c.mu.Unlock()
}

// PauseForInput parks the node on a worker question (vetting.NodeControl).
func (c *nodeControl) PauseForInput(question string) { c.markPaused(PauseAwaitingInput, question) }

// resume clears the pause so the node can re-enter its gate loop.
func (c *nodeControl) resume() {
	c.mu.Lock()
	c.paused, c.reason = false, ""
	c.mu.Unlock()
	// running, not queued: the node is live, and it's the table's legal target.
	c.persistStatus(StatusRunning, "", "")
}

func (c *nodeControl) persistStatus(status NodeStatus, reason PauseReason, question string) {
	if c == nil || c.store == nil {
		return
	}
	if err := c.store.SetNodeStatusForChat(context.Background(), c.chatID, c.nodeID, string(status), string(reason), question); err != nil {
		slog.Warn("nodeControl: persist status failed", "component", "dag",
			"chat", c.chatID, "node", c.nodeID, "status", status, "err", err)
	}
}

// persistQueue mirrors the steer queue to dag_nodes.queued_messages. Caller holds no lock.
func (c *nodeControl) persistQueue() {
	if c == nil || c.store == nil {
		return
	}
	b, err := json.Marshal(c.snapshotQueue())
	if err != nil {
		return
	}
	if err := c.store.SetNodeQueue(context.Background(), c.chatID, c.nodeID, string(b)); err != nil {
		slog.Warn("nodeControl: persist queue failed", "component", "dag",
			"chat", c.chatID, "node", c.nodeID, "err", err)
	}
}

// enqueue appends a new queued message and returns a copy, delivering
// straight into a live round when possible.
func (c *nodeControl) enqueue(text string) queuedMsg {
	c.mu.Lock()
	live := c.liveSteer
	c.mu.Unlock()
	delivered := live != nil && live(text)

	c.mu.Lock()
	defer c.mu.Unlock()
	m := &queuedMsg{ID: newMsgID(), Text: text, Delivered: delivered, CreatedAt: time.Now().UTC()}
	c.queue = append(c.queue, m)
	return *m
}

// restore rebuilds a fresh control's steer queue from dag_nodes so a node
// re-registered after a restart still carries its undelivered messages. A
// persisted pause is NOT rehydrated - a registering node is starting, so the
// pause is cleared instead (paused|needs_input → running, the legal resume
// target), keeping row and memory agreed before the first gate check.
func (c *nodeControl) restore() {
	if c.store == nil {
		return
	}
	status, _, _, queueJSON, err := c.store.GetNodeState(context.Background(), c.chatID, c.nodeID)
	if err != nil {
		slog.Warn("nodeControl: restore failed", "component", "dag", "chat", c.chatID, "node", c.nodeID, "err", err)
		return
	}
	if IsPaused(NodeStatus(status)) {
		c.persistStatus(StatusRunning, "", "")
	}
	var msgs []queuedMsg
	if queueJSON != "" {
		_ = json.Unmarshal([]byte(queueJSON), &msgs)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range msgs {
		if !msgs[i].Delivered {
			m := msgs[i]
			c.queue = append(c.queue, &m)
		}
	}
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
	paused    map[string]map[string]PauseReason  // chatID → nodeID → why it paused this run; persists past unregister, same reason as cancelled
	overrides map[string]map[string]string       // chatID → nodeID → pending prompt edit for a not-yet-started node (see graph.go's effectiveNode.Task)
	store     NodeStateStore
}

func newRunControls() *runControls {
	return &runControls{
		m:         map[string]map[string]*nodeControl{},
		cancelled: map[string]map[string]bool{},
		paused:    map[string]map[string]PauseReason{},
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
	_, ok := r.paused[chatID][nodeID]
	return ok
}

// markPausedSticky records the pause reason past unregister.
func (r *runControls) markPausedSticky(chatID, nodeID string, reason PauseReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused[chatID] == nil {
		r.paused[chatID] = map[string]PauseReason{}
	}
	r.paused[chatID][nodeID] = reason
}

// clearPausedSticky forgets a pause once the node is started again.
func (r *runControls) clearPausedSticky(chatID, nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.paused[chatID], nodeID)
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
	c = &nodeControl{store: r.store, chatID: chatID, nodeID: nodeID}
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

// register builds the control and rehydrates its persisted queue. Starting is
// the resume transition: restore() clears any persisted pause, and the sticky
// pause flag goes with it so the stream can't relabel the new run as paused.
func (r *runControls) register(chatID, nodeID string) (*nodeControl, string, bool) {
	c, override, ok := r.registerAndTakeOverride(chatID, nodeID)
	c.restore()
	r.clearPausedSticky(chatID, nodeID)
	return c, override, ok
}

// SetNodeStateStore wires write-through persistence for the node state
// machine. Nil (the default) keeps every control in memory only.
func (e *Executor) SetNodeStateStore(s NodeStateStore) { e.controls.store = s }

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

// PauseNode suspends a running node at its next gate boundary. reason
// distinguishes a human pause from a shutdown drain; HITL pauses itself
// through the same seam (nodeControl.PauseForInput). Empty reason = user.
func (e *Executor) PauseNode(chatID, nodeID string, reason PauseReason) bool {
	if reason == "" {
		reason = PauseUser
	}
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	c.markPaused(reason, "")
	e.controls.markPausedSticky(chatID, nodeID, reason)
	return true
}

// StartNode clears a node's pause so its graph can be re-entered (the
// re-entry itself is the orchestrator's - see Orchestrator.StartNode).
// Returns the reason it was paused for, so the caller knows whether the
// incoming message is an answer to a question (awaiting_input) or nothing at
// all (user/shutdown).
func (e *Executor) StartNode(chatID, nodeID string) (PauseReason, bool) {
	reason := e.NodePauseReason(chatID, nodeID)
	if c := e.controls.get(chatID, nodeID); c != nil {
		if reason == "" {
			reason = c.PauseReason()
		}
		c.resume()
	}
	e.controls.clearPausedSticky(chatID, nodeID)
	return reason, true
}

// StopNode cancels a node into the terminal cancelled state.
func (e *Executor) StopNode(chatID, nodeID string) bool { return e.CancelNode(chatID, nodeID) }

// NodePauseReason reports why a node is paused ("" if it isn't), surviving unregister.
func (e *Executor) NodePauseReason(chatID, nodeID string) PauseReason {
	e.controls.mu.Lock()
	defer e.controls.mu.Unlock()
	return e.controls.paused[chatID][nodeID]
}

// SetNodeLiveSteer registers a live round's forward hook (#998). No-op if unregistered.
func (e *Executor) SetNodeLiveSteer(chatID, nodeID string, f func(text string) bool) {
	if c := e.controls.get(chatID, nodeID); c != nil {
		c.setLiveSteer(f)
	}
}

// ClearNodeLiveSteer un-registers the live-delivery hook at round end.
func (e *Executor) ClearNodeLiveSteer(chatID, nodeID string) {
	if c := e.controls.get(chatID, nodeID); c != nil {
		c.clearLiveSteer()
	}
}

// QueueNodeMessage appends a message to a running node's queue.
func (e *Executor) QueueNodeMessage(chatID, nodeID, text string) (QueuedMessage, bool) {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return QueuedMessage{}, false
	}
	m := c.enqueue(text)
	c.persistQueue()
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
	ok := c.editQueued(messageID, text)
	if ok {
		c.persistQueue()
	}
	return ok
}

// RemoveQueuedMessage drops a not-yet-delivered queued message.
func (e *Executor) RemoveQueuedMessage(chatID, nodeID, messageID string) bool {
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	ok := c.removeQueued(messageID)
	if ok {
		c.persistQueue()
	}
	return ok
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
