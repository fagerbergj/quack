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

// ApplyOutcome matches memories by provenance chat_id and applies o to every
// one of them that isn't already invalidated (sticky: nothing revives an
// invalidated memory, and invalidating twice is idempotent). Returns the
// count touched. Reinforce bumps reinforcement_count and promotes
// unverified→reinforced; invalidate stamps invalidated_at/invalidation_reason.
// Every touched memory writes one memory_ops row (actor=outcome-feedback).
func (s *Store) ApplyOutcome(ctx context.Context, chatID string, o OutcomeSignal) (int, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return 0, nil
	}
	if o.Kind == OutcomeInvalidated && strings.TrimSpace(o.Reason) == "" {
		return 0, fmt.Errorf("memory: ApplyOutcome invalidate requires a reason")
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
	s.log.Debug("apply outcome", "chat_id", chatID, "kind", o.Kind, "touched", len(ids))
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
