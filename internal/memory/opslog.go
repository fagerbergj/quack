package memory

import (
	"context"
	"time"
)

// OpsLogOp names one memory_ops audit row's transition.
type OpsLogOp string

const (
	OpAdd        OpsLogOp = "add"
	OpUpdate     OpsLogOp = "update"
	OpDelete     OpsLogOp = "delete"
	OpReinforce  OpsLogOp = "reinforce"
	OpInvalidate OpsLogOp = "invalidate"
)

// OpsLogActor names who caused a memory_ops transition.
type OpsLogActor string

const (
	ActorConsolidator    OpsLogActor = "consolidator"
	ActorOutcomeFeedback OpsLogActor = "outcome-feedback"
	ActorHuman           OpsLogActor = "human"
	ActorRun             OpsLogActor = "run"
)

// OpsLog persists an append-only audit trail of memory lifecycle transitions
// (design doc §3's memory_ops table). internal/memory can't import
// internal/store - the server bootstrap (internal/serve) wires a concrete
// implementation backed by internal/store.Store at construction time (see
// Store.SetOpsLog).
type OpsLog interface {
	LogMemoryOp(ctx context.Context, memoryID string, op OpsLogOp, actor OpsLogActor, reason string) error
	// PruneMemoryOps hard-deletes memory_ops rows older than cutoff (design
	// doc §6's bound on unbounded audit-trail growth) and reports how many
	// were removed - the retention sweep's counterpart to the point-level
	// hard-delete it runs alongside.
	PruneMemoryOps(ctx context.Context, cutoff time.Time) (int, error)
}

// logOp is best-effort: an audit-write failure must never fail the memory
// write it's recording, or unwired (nil opsLog, e.g. most tests) callers.
func (s *Store) logOp(ctx context.Context, memoryID string, op OpsLogOp, actor OpsLogActor, reason string) {
	if s.opsLog == nil {
		return
	}
	if err := s.opsLog.LogMemoryOp(ctx, memoryID, op, actor, reason); err != nil {
		s.log.Warn("memory_ops audit write failed", "memory_id", memoryID, "op", op, "actor", actor, "err", err)
	}
}
