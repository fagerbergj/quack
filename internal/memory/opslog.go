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
	// OpVote: a judge or human per-memory vote. Recall delivery isn't logged here; the ledger's
	// memory.recall entry is the source of truth.
	OpVote OpsLogOp = "vote"
	// OpDemote: a forgetting-rule demotion to unverified. The memory stays live and a later
	// supported vote re-promotes it.
	OpDemote OpsLogOp = "demote"
)

// ReasonSupportDecayed is the memory_ops reason every demote writes, whichever rule triggered it.
const ReasonSupportDecayed = "support decayed"

// OpsLogActor names who caused a memory_ops transition.
type OpsLogActor string

const (
	ActorConsolidator    OpsLogActor = "consolidator"
	ActorOutcomeFeedback OpsLogActor = "outcome-feedback"
	ActorHuman           OpsLogActor = "human"
	ActorRun             OpsLogActor = "run"
	ActorJudge           OpsLogActor = "judge"
	// ActorSweep: the forgetting-rule sweep (ages memories out, unlike ActorConsolidator's merges).
	ActorSweep OpsLogActor = "sweep"
	// ActorRescope: quack memory rescope, moving a role:* point into its repo:* bucket.
	ActorRescope OpsLogActor = "rescope"
)

// OpsLog persists the append-only memory_ops audit trail. internal/serve wires an internal/store-backed
// implementation via Store.SetOpsLog, since this package can't import internal/store.
type OpsLog interface {
	LogMemoryOp(ctx context.Context, memoryID string, op OpsLogOp, actor OpsLogActor, reason string) error
	// PruneMemoryOps hard-deletes memory_ops rows older than cutoff and reports how many, alongside
	// the retention sweep's point deletes.
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
