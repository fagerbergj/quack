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

// transitions: legal node-status state machine.
var transitions = map[NodeStatus]map[NodeStatus]bool{
	StatusQueued: {
		StatusQueued:    true, // idempotent re-queue (initial persist, retry fan-out)
		StatusRunning:   true,
		StatusCancelled: true,
		StatusFailed:    true,
	},
	StatusRunning: {
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
