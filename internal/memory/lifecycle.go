package memory

import (
	"context"
	"fmt"
	"strings"
)

// Status is a memory's epistemic tier (design doc §3, phase 2). A point
// written before this phase carries no status at all - callers treat "" the
// same as StatusUnverified everywhere (recall filter, tier prefix), so no
// backfill migration is needed.
type Status string

const (
	StatusUnverified  Status = "unverified"
	StatusReinforced  Status = "reinforced"
	StatusInvalidated Status = "invalidated"
)

// OutcomeKind is the deterministic oracle's verdict on a chat's minted memories.
type OutcomeKind string

const (
	OutcomeReinforced  OutcomeKind = "reinforced"
	OutcomeInvalidated OutcomeKind = "invalidated"
)

// OutcomeSignal is one outcome event (design doc §5) - PR merged/closed, a
// human delete, or any future deterministic oracle. Reason is required for
// OutcomeInvalidated (it becomes invalidation_reason on every touched memory)
// and ignored for OutcomeReinforced.
type OutcomeSignal struct {
	Kind   OutcomeKind
	Reason string
}

// OutcomeReasonClosedUnmerged is the fixed invalidation_reason a subject
// (PR/issue) closed without merging stamps on every memory it minted -
// callers and tests compare against this constant rather than a literal.
const OutcomeReasonClosedUnmerged = "subject closed unmerged"

// ApplyOutcome matches memories by provenance chat_id and applies o to every
// one of them that isn't already invalidated (sticky: nothing revives an
// invalidated memory, and invalidating twice is idempotent). Returns the
// count touched. Reinforce bumps reinforcement_count and promotes
// unverified→reinforced; invalidate stamps invalidated_at/invalidation_reason.
// Every touched memory writes one memory_ops row (actor=outcome-feedback).
//
// updateStatus is read-then-write per row, not serializable: two concurrent
// calls for the same chatID could both read the same reinforcement_count and
// under-count by one. Invalidate is idempotent either way; only a
// simultaneous double-reinforce is affected, and only quack's own webhook
// dedup keeps that rare in practice.
func (s *Store) ApplyOutcome(ctx context.Context, chatID string, o OutcomeSignal) (int, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return 0, nil
	}
	if o.Kind == OutcomeInvalidated && strings.TrimSpace(o.Reason) == "" {
		return 0, fmt.Errorf("memory: ApplyOutcome invalidate requires a reason")
	}
	if o.Kind != OutcomeReinforced && o.Kind != OutcomeInvalidated {
		return 0, fmt.Errorf("memory: ApplyOutcome: unknown outcome kind %q", o.Kind)
	}
	ids, err := s.idx.updateStatus(ctx, chatID, o)
	if err != nil {
		return 0, fmt.Errorf("memory: apply outcome: %w", err)
	}
	op := OpReinforce
	if o.Kind == OutcomeInvalidated {
		op = OpInvalidate
	}
	for _, id := range ids {
		s.logOp(ctx, id, op, ActorOutcomeFeedback, o.Reason)
	}
	if len(ids) > 0 {
		s.log.Info("apply outcome", "chat_id", chatID, "kind", o.Kind, "touched", len(ids))
	} else {
		s.log.Debug("apply outcome", "chat_id", chatID, "kind", o.Kind, "touched", 0)
	}
	return len(ids), nil
}

// tierPrefix is the compact plain-string epistemic tag prepended to a
// recalled memory's text, same convention as citeReasonLegend (#822). Any
// status other than "reinforced" - including "" (pre-lifecycle points) and
// "invalidated" (shouldn't reach here; recall already excludes it) - reads
// as unverified rather than silently omitting the tag.
func tierPrefix(status string, reinforcementCount int) string {
	if status == string(StatusReinforced) {
		return fmt.Sprintf("[reinforced ×%d] ", reinforcementCount)
	}
	return "[unverified, single run] "
}
