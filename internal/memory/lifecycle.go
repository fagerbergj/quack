package memory

import (
	"context"
	"fmt"
	"strings"
)

// Status is a memory's epistemic tier. "" (pre-lifecycle point) reads as StatusUnverified everywhere,
// so no backfill is needed.
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

// OutcomeSignal is one outcome event (PR merged/closed, human delete). Reason becomes
// invalidation_reason for OutcomeInvalidated and is ignored for OutcomeReinforced.
type OutcomeSignal struct {
	Kind   OutcomeKind
	Reason string
}

// OutcomeReasonClosedUnmerged is the invalidation_reason a subject closed without merging stamps
// on the memories it minted.
const OutcomeReasonClosedUnmerged = "subject closed unmerged"

// ApplyOutcome applies o to the chat's recalled ids, skipping invalidated (sticky) ones, one memory_ops row each.
// Reinforce adds an upvote but never sets tier (a merge isn't proof); invalidate skips verified memories.
func (s *Store) ApplyOutcome(ctx context.Context, ids []string, o OutcomeSignal) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if o.Kind == OutcomeInvalidated && strings.TrimSpace(o.Reason) == "" {
		return 0, fmt.Errorf("memory: ApplyOutcome invalidate requires a reason")
	}
	if o.Kind != OutcomeReinforced && o.Kind != OutcomeInvalidated {
		return 0, fmt.Errorf("memory: ApplyOutcome: unknown outcome kind %q", o.Kind)
	}
	touched, err := s.updateStatus(ctx, ids, o)
	if err != nil {
		return 0, fmt.Errorf("memory: apply outcome: %w", err)
	}
	op := OpReinforce
	if o.Kind == OutcomeInvalidated {
		op = OpInvalidate
	}
	for _, id := range touched {
		s.logOp(ctx, id, op, ActorOutcomeFeedback, o.Reason)
	}
	if len(touched) > 0 {
		s.log.Info("apply outcome", "kind", o.Kind, "recalled", len(ids), "touched", len(touched))
	} else {
		s.log.Debug("apply outcome", "kind", o.Kind, "recalled", len(ids), "touched", 0)
	}
	return len(touched), nil
}

// OutcomeReasonNetScore is the fixed invalidation_reason a memory's net
// vote score dropping to invalidateThreshold or below stamps.
const OutcomeReasonNetScore = "net score"

// Vote is one judge's or human's verdict on a recalled memory. NotRelevant moves no score but counts
// toward DefaultNotRelevantThreshold and always logs a memory_ops row.
type Vote struct {
	MemoryID string
	Vote     string // "supported" | "contradicted" | "not_relevant"
	Reason   string
	Actor    OpsLogActor
}

const (
	VoteSupported    = "supported"
	VoteContradicted = "contradicted"
	VoteNotRelevant  = "not_relevant"
)

// DefaultInvalidateThreshold: a net score (upvotes-downvotes) at or below this invalidates.
const DefaultInvalidateThreshold = -2

// DefaultNotRelevantThreshold: this many not_relevant votes with zero supported invalidates.
const DefaultNotRelevantThreshold = 3

// OutcomeReasonRecalledWithoutSupport is the invalidation_reason for hitting DefaultNotRelevantThreshold.
const OutcomeReasonRecalledWithoutSupport = "recalled without support"

// ApplyVotes applies votes via computeVoteDelta, collapsing repeats of one id to the last so a round
// can't double-count. Invalidated memories are skipped; each applied vote writes a memory_ops row.
func (s *Store) ApplyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) (int, error) {
	if len(votes) == 0 {
		return 0, nil
	}
	deduped := dedupeVotes(votes)
	touched, err := s.applyVotes(ctx, deduped, invalidateThreshold)
	if err != nil {
		return 0, fmt.Errorf("memory: apply votes: %w", err)
	}
	byID := make(map[string]Vote, len(deduped))
	for _, v := range deduped {
		byID[v.MemoryID] = v
	}
	for _, id := range touched {
		v := byID[id]
		s.logOp(ctx, id, OpVote, v.Actor, v.Reason)
	}
	s.log.Info("apply votes", "votes", len(votes), "touched", len(touched))
	return len(touched), nil
}

// HumanVoteUp/Down/None: the request-body values for SetHumanVote.
const (
	HumanVoteUp   = "up"
	HumanVoteDown = "down"
	HumanVoteNone = "none"
)

// SetHumanVote casts or clears the deployment user's vote on id. Unlike ApplyVotes it is idempotent and
// reversible: the stored human_vote is the source of truth for what to undo.
func (s *Store) SetHumanVote(ctx context.Context, id, vote string) error {
	switch vote {
	case HumanVoteUp, HumanVoteDown, HumanVoteNone:
	default:
		return fmt.Errorf("memory: SetHumanVote: unknown vote %q", vote)
	}
	touched, err := s.setHumanVote(ctx, id, vote, DefaultInvalidateThreshold)
	if err != nil {
		return fmt.Errorf("memory: set human vote: %w", err)
	}
	if !touched {
		return ErrMemoryNotFound
	}
	s.logOp(ctx, id, OpVote, ActorHuman, "")
	s.log.Info("set human vote", "id", id, "vote", vote)
	return nil
}

// computeHumanVoteDelta undoes oldVote's effect and applies newVote's, the toggle-safe twin of
// computeVoteDelta. A human up counts as support; supported is floored at 0.
func computeHumanVoteDelta(upvotes, downvotes, supported int, oldVote, newVote, now string, invalidateThreshold int) voteDelta {
	switch oldVote {
	case HumanVoteUp:
		upvotes--
		supported--
	case HumanVoteDown:
		downvotes--
	}
	switch newVote {
	case HumanVoteUp:
		upvotes++
		supported++
	case HumanVoteDown:
		downvotes++
	}
	if upvotes < 0 {
		upvotes = 0
	}
	if downvotes < 0 {
		downvotes = 0
	}
	if supported < 0 {
		supported = 0
	}
	d := voteDelta{Upvotes: upvotes, Downvotes: downvotes, Supported: supported, VoteScore: upvotes - downvotes, Tier: tierFromSupported(supported)}
	if newVote == HumanVoteUp {
		d.LastUpvotedAt = now
	}
	if d.VoteScore <= invalidateThreshold {
		d.Invalidate = true
	}
	return d
}

// TierUnverified/TierVerified: vote-based tier, independent of Status (see tierFromSupported).
const (
	TierUnverified = "unverified"
	TierVerified   = "verified"
)

// tierFromSupported is verified iff any judge/human support is on record; reinforcement never counts.
func tierFromSupported(supported int) string {
	if supported >= 1 {
		return TierVerified
	}
	return TierUnverified
}

// voteDelta is one vote's backend-agnostic result; sqlite and qdrant each build their write from it.
type voteDelta struct {
	Upvotes, Downvotes, Supported, NotRelevant, VoteScore int
	Tier                                                  string
	LastUpvotedAt                                         string // "" = unchanged
	Invalidate                                            bool
	InvalidateReason                                      string // set only when Invalidate is true
}

// computeVoteDelta is the one place vote arithmetic lives. Contradicted can invalidate at invalidateThreshold,
// not_relevant at DefaultNotRelevantThreshold with zero support; tier is always recomputed.
func computeVoteDelta(upvotes, downvotes, supported, notRelevant int, now string, v Vote, invalidateThreshold int) voteDelta {
	d := voteDelta{Upvotes: upvotes, Downvotes: downvotes, Supported: supported, NotRelevant: notRelevant, VoteScore: upvotes - downvotes}
	switch v.Vote {
	case VoteSupported:
		d.Upvotes++
		d.Supported++
		d.VoteScore++
		d.LastUpvotedAt = now
	case VoteContradicted:
		d.Downvotes++
		d.VoteScore--
		if d.VoteScore <= invalidateThreshold {
			d.Invalidate = true
			d.InvalidateReason = OutcomeReasonNetScore
		}
	case VoteNotRelevant:
		d.NotRelevant++
		if d.NotRelevant >= DefaultNotRelevantThreshold && d.Supported == 0 {
			d.Invalidate = true
			d.InvalidateReason = OutcomeReasonRecalledWithoutSupport
		}
	}
	d.Tier = tierFromSupported(d.Supported)
	return d
}

// reinforcedVoteScore is shared so both backends keep vote_score == upvotes - downvotes on reinforce.
func reinforcedVoteScore(upvotes, downvotes int) int { return (upvotes + 1) - downvotes }

// dedupeVotes keeps the LAST vote per memory id, in stable order for deterministic output.
func dedupeVotes(votes []Vote) []Vote {
	last := make(map[string]Vote, len(votes))
	var order []string
	for _, v := range votes {
		if _, ok := last[v.MemoryID]; !ok {
			order = append(order, v.MemoryID)
		}
		last[v.MemoryID] = v
	}
	out := make([]Vote, len(order))
	for i, id := range order {
		out[i] = last[id]
	}
	return out
}

// tierPrefix is the epistemic tag prepended to a recalled memory. It reads tier/supported, not status, so a
// memory merely reinforced by a merge presents as unverified.
func tierPrefix(tier string, supported int) string {
	if tier == TierVerified {
		return fmt.Sprintf("[verified, supported ×%d] ", supported)
	}
	return "[unverified] "
}
