package dag

import "sort"

// NodeStatus is the canonical node lifecycle state, persisted as string.
type NodeStatus string

const (
	StatusQueued     NodeStatus = "queued"
	StatusRunning    NodeStatus = "running"
	StatusNeedsInput NodeStatus = "needs_input"
	StatusPaused     NodeStatus = "paused"
	StatusDone       NodeStatus = "done"
	StatusFailed     NodeStatus = "failed"
	StatusCancelled  NodeStatus = "cancelled"
)

var transitions = map[NodeStatus]map[NodeStatus]bool{
	StatusQueued: {
		StatusQueued:    true, // idempotent re-queue (initial persist, retry fan-out)
		StatusRunning:   true,
		StatusCancelled: true,
		StatusFailed:    true,
	},
	StatusRunning: {
		// A running node re-queues at every admission wait, not just its
		// first: worker->judge and judge->worker slot swaps wait again mid-run.
		StatusQueued:     true,
		StatusPaused:     true,
		StatusNeedsInput: true,
		StatusDone:       true,
		StatusFailed:     true,
		StatusCancelled:  true,
	},
	StatusPaused: {
		StatusRunning:   true,
		StatusCancelled: true,
	},
	StatusNeedsInput: {
		StatusRunning:   true,
		StatusCancelled: true,
	},
	StatusDone: {
		StatusQueued: true,
	},
	StatusFailed: {
		StatusQueued: true,
	},
	StatusCancelled: {
		StatusQueued: true,
	},
}

// CanTransition reports whether from → to is a legal transition (empty from defaults to queued).
func CanTransition(from, to NodeStatus) bool {
	if from == "" {
		from = StatusQueued
	}
	return transitions[from][to]
}

// CanPersist is CanTransition plus a retry's re-run: RetryNode starts a finished node straight
// into running with no queued step, so the persisted row and dag_node record accept that too.
func CanPersist(from, to NodeStatus) bool {
	if to == StatusRunning && (from == StatusDone || from == StatusFailed || from == StatusCancelled) {
		return true
	}
	return CanTransition(from, to)
}

// AllowedTargets returns sorted legal target statuses for 409 responses.
func AllowedTargets(from NodeStatus) []NodeStatus {
	if from == "" {
		from = StatusQueued
	}
	out := make([]NodeStatus, 0, len(transitions[from]))
	for to, ok := range transitions[from] {
		if ok {
			out = append(out, to)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PauseReason says why a node sits in StatusPaused; empty for any other status.
type PauseReason string

const (
	PauseUser          PauseReason = "user"           // a human hit pause
	PauseShutdown      PauseReason = "shutdown"       // the process is draining (PR 2)
	PauseAwaitingInput PauseReason = "awaiting_input" // HITL: the worker asked the user something
)

// IsPaused reports whether a persisted status means "suspended, resumable". StatusNeedsInput, the
// legacy wire spelling of paused/awaiting_input, is still emitted by REST, so both answer here.
func IsPaused(s NodeStatus) bool { return s == StatusPaused || s == StatusNeedsInput }
