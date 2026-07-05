package dag

import "sort"

// NodeStatus is the canonical node lifecycle state, mirroring openapi.yaml's
// NodeStatus enum (schema.NodeStatus is the generated wire type; this is the
// server-side domain type every status write routes through — see
// CanTransition). The store persists its string value directly.
type NodeStatus string

const (
	StatusQueued     NodeStatus = "queued"
	StatusRunning    NodeStatus = "running"
	StatusNeedsInput NodeStatus = "needs_input"
	StatusDone       NodeStatus = "done"
	StatusFailed     NodeStatus = "failed"
	StatusCancelled  NodeStatus = "cancelled"
)

// transitions is the legal edge set of the node-status state machine. Every
// status write in the system (executor, gate, control, store) routes through
// CanTransition — an edge missing here is a bug (logged), not a silent write.
//
//	queued      → running (node dispatched), cancelled (user cancel), failed (stale-on-restart)
//	running     → running (steer: re-run in place with guidance), needs_input (HITL pause),
//	              done, failed, cancelled
//	needs_input → running (resumed with the user's answer), cancelled
//	done        → queued (retry)
//	failed      → queued (retry)
//	cancelled   → queued (retry)
var transitions = map[NodeStatus]map[NodeStatus]bool{
	StatusQueued: {
		StatusRunning:   true,
		StatusCancelled: true,
		StatusFailed:    true,
	},
	StatusRunning: {
		StatusRunning:    true,
		StatusNeedsInput: true,
		StatusDone:       true,
		StatusFailed:     true,
		StatusCancelled:  true,
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

// CanTransition reports whether from → to is a legal node-status transition.
// An empty/unknown `from` (a node with no persisted row yet) is treated as
// StatusQueued, its implicit default.
func CanTransition(from, to NodeStatus) bool {
	if from == "" {
		from = StatusQueued
	}
	return transitions[from][to]
}

// AllowedTargets returns the sorted legal target statuses from a given status,
// for a 409 response body naming them. Empty/unknown `from` defaults to
// StatusQueued, matching CanTransition.
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
