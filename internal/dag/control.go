package dag

import (
	"strings"
	"sync"
)

// nodeControl is the live handle to one running gated node. It implements
// vetting.NodeControl: cancel and steer are cooperative, taking effect at the
// gate's stage boundaries (not mid-model-call — see docs Phase 3c).
type nodeControl struct {
	mu        sync.Mutex
	cancelled bool
	steer     string
}

func (c *nodeControl) Cancelled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelled
}

func (c *nodeControl) TakeSteer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.steer
	c.steer = ""
	return s
}

func (c *nodeControl) markCancelled() {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()
}

func (c *nodeControl) setSteer(g string) {
	c.mu.Lock()
	c.steer = g
	c.mu.Unlock()
}

// runControls tracks per-chat, per-node controls for active runs so the
// orchestrator can cancel or steer a single running node while the DAG runs.
// ponytail: a plain mutex-guarded map — one active run per chat, a few nodes.
type runControls struct {
	mu        sync.Mutex
	m         map[string]map[string]*nodeControl // chatID → nodeID → control (live)
	cancelled map[string]map[string]bool         // chatID → nodeID → user-cancelled; persists after the control is unregistered so the stream can mark the node "cancelled" (not "failed")
	steers    map[string]map[string][]string     // chatID → nodeID → guidance per delivered steer, in order; generation N (the -sN run suffix) reads steers[N-1]
}

func newRunControls() *runControls {
	return &runControls{m: map[string]map[string]*nodeControl{}, cancelled: map[string]map[string]bool{}}
}

// wasCancelled reports whether a node was user-cancelled this run (survives the
// control's unregister, unlike get()).
func (r *runControls) wasCancelled(chatID, nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled[chatID][nodeID]
}

// resetCancelled clears a chat's user-cancelled flags. Called at the start of
// each new turn so a cancelled node ID (n1, n2, … are reused across plans) can't
// leak into the next turn's same-ID node and mark it "stopped".
func (r *runControls) resetCancelled(chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancelled, chatID)
	delete(r.steers, chatID)
}

// recordSteer appends a delivered steer's guidance for the node, so the stream
// can put the text on the node_steered event when the -sN re-run appears.
func (r *runControls) recordSteer(chatID, nodeID, guidance string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.steers == nil {
		r.steers = map[string]map[string][]string{}
	}
	if r.steers[chatID] == nil {
		r.steers[chatID] = map[string][]string{}
	}
	r.steers[chatID][nodeID] = append(r.steers[chatID][nodeID], guidance)
}

// steerGuidance returns the guidance of the node's Nth steer (1-based — the -sN
// run-ID suffix), or "" when unknown.
func (r *runControls) steerGuidance(chatID, nodeID string, n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g := r.steers[chatID][nodeID]; n >= 1 && n <= len(g) {
		return g[n-1]
	}
	return ""
}

func (r *runControls) register(chatID, nodeID string) *nodeControl {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[chatID] == nil {
		r.m[chatID] = map[string]*nodeControl{}
	}
	c := &nodeControl{}
	r.m[chatID][nodeID] = c
	return c
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
// boundary — a worker deep in a tool loop can be many minutes from one, which is
// what made cancel look like a no-op.
//
// It reads the same wasCancelled flag the gate reads, and outlives the node's
// control registration on purpose: an in-flight tool call that lands just after
// the node unregisters must still be told to stop.
func (e *Executor) NodeCancelled(chatID, nodeID string) bool {
	return e.controls.wasCancelled(chatID, nodeID)
}

// SteerNode re-runs one running node with new guidance at its next gate-stage
// boundary. Returns false if no such node is running.
func (e *Executor) SteerNode(chatID, nodeID, guidance string) bool {
	if strings.TrimSpace(guidance) == "" {
		return false
	}
	c := e.controls.get(chatID, nodeID)
	if c == nil {
		return false
	}
	c.setSteer(guidance)
	e.controls.recordSteer(chatID, nodeID, guidance)
	return true
}
