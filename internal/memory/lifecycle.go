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

// ApplyOutcome applies o to every id in ids that isn't already invalidated
// (sticky: nothing revives an invalidated memory, and invalidating twice is
// idempotent). ids is the chat's RECALLED set (epic P1: reinforcement
// is recall-based, not birth-based) - the caller folds the chat's ledger for
// memory.recall entries and passes their ids; minting still sets provenance,
// but no longer drives what gets reinforced/invalidated. Returns the count
// touched. Reinforce is +1 upvote (see applyVotes) with actor
// outcome-feedback; invalidate stamps invalidated_at/invalidation_reason on
// every unverified id (a verified memory recalled into a closed-unmerged
// chat gets no vote, not an invalidation - it's never demoted by this path).
// Every touched memory writes one memory_ops row.
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
	touched, err := s.idx.updateStatus(ctx, ids, o)
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

// Vote is one judge's (or human's) verdict on a recalled memory (epic #1255
// P1). NotRelevant carries no score delta but still logs a memory_ops row -
// an audit trail of what the judge considered, not just what it moved.
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

// DefaultInvalidateThreshold: a memory's net score (upvotes-downvotes) at or
// below this invalidates it (design decision #1255).
const DefaultInvalidateThreshold = -2

// ApplyVotes applies each vote to its memory (supported: +1 upvote,
// last_upvoted_at, tier→verified; contradicted: +1 downvote, and a net score
// <= invalidateThreshold also invalidates; not_relevant: no score change).
// Duplicate votes for the same id in one call are collapsed to the last one
// (a judge that names a memory twice in one round should not double-count
// it). Sticky: an already-invalidated memory is skipped. Every applied vote
// (including not_relevant) writes one memory_ops row (op=vote).
func (s *Store) ApplyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) (int, error) {
	if len(votes) == 0 {
		return 0, nil
	}
	deduped := dedupeVotes(votes)
	touched, err := s.idx.applyVotes(ctx, deduped, invalidateThreshold)
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

// SetHumanVote casts or clears the single human deployment's own vote on id
// (epic P4). Unlike ApplyVotes (judge, additive-only), this is
// idempotent under repeated identical calls and reversible: voting the same
// direction twice is a no-op, voting the opposite direction flips it, and
// "none" removes whatever the caller's prior vote was - the point's stored
// human_vote is always the source of truth for what to undo.
func (s *Store) SetHumanVote(ctx context.Context, id, vote string) error {
	switch vote {
	case HumanVoteUp, HumanVoteDown, HumanVoteNone:
	default:
		return fmt.Errorf("memory: SetHumanVote: unknown vote %q", vote)
	}
	touched, err := s.idx.setHumanVote(ctx, id, vote, DefaultInvalidateThreshold)
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

// computeHumanVoteDelta re-derives upvotes/downvotes/vote_score/tier by
// undoing oldVote's effect (if any) and applying newVote's - the toggle-safe
// twin of computeVoteDelta, which only ever adds. Tier is sticky-verified
// (never demoted, matching computeVoteDelta/#1255 P1) even if undoing the
// vote that earned it drops upvotes back to 0.
func computeHumanVoteDelta(upvotes, downvotes int, tier, oldVote, newVote, now string, invalidateThreshold int) voteDelta {
	switch oldVote {
	case HumanVoteUp:
		upvotes--
	case HumanVoteDown:
		downvotes--
	}
	switch newVote {
	case HumanVoteUp:
		upvotes++
	case HumanVoteDown:
		downvotes++
	}
	if upvotes < 0 {
		upvotes = 0
	}
	if downvotes < 0 {
		downvotes = 0
	}
	d := voteDelta{Upvotes: upvotes, Downvotes: downvotes, VoteScore: upvotes - downvotes, Tier: tier}
	if d.Tier == "" {
		d.Tier = TierUnverified
	}
	if newVote == HumanVoteUp {
		d.Tier = TierVerified
		d.LastUpvotedAt = now
	}
	if d.VoteScore <= invalidateThreshold {
		d.Invalidate = true
	}
	return d
}

// TierUnverified/TierVerified: a memory's vote-based tier (epic P1),
// independent of Status (which tracks invalidation, not votes). Verified is
// sticky once reached - a downvote can invalidate via net score, but never
// demotes a memory back to unverified.
const (
	TierUnverified = "unverified"
	TierVerified   = "verified"
)

// voteDelta is the field-level result of applying one vote to a memory's
// current upvotes/downvotes/vote_score - the backend-agnostic core both
// sqlite (a map of column updates) and qdrant (a payload SetPayload) build
// their own write from.
type voteDelta struct {
	Upvotes, Downvotes, VoteScore int
	Tier                          string
	LastUpvotedAt                 string // "" = unchanged
	Invalidate                    bool
}

// computeVoteDelta is the one place vote arithmetic lives. supported: +1
// upvote, tier verified, last_upvoted_at stamped. contradicted: +1
// downvote; net score <= invalidateThreshold also invalidates.
// not_relevant: no score change (still returns the unchanged counts so the
// caller has a uniform write, and still gets logged by the caller).
func computeVoteDelta(upvotes, downvotes int, tier, now string, v Vote, invalidateThreshold int) voteDelta {
	d := voteDelta{Upvotes: upvotes, Downvotes: downvotes, VoteScore: upvotes - downvotes, Tier: tier}
	if d.Tier == "" {
		d.Tier = TierUnverified
	}
	switch v.Vote {
	case VoteSupported:
		d.Upvotes++
		d.VoteScore++
		d.Tier = TierVerified
		d.LastUpvotedAt = now
	case VoteContradicted:
		d.Downvotes++
		d.VoteScore--
		if d.VoteScore <= invalidateThreshold {
			d.Invalidate = true
		}
	}
	return d
}

// reinforcedVoteScore is the one shared computation both backends' outcome
// reinforce path calls for the new vote_score - a bug had
// qdrant recompute this from upvotes/downvotes while sqlite incremented its
// own stored vote_score by 1, diverging once a memory carried any
// downvotes. Both backends now call this instead of deriving it locally, so
// vote_score == upvotes - downvotes holds identically on either.
func reinforcedVoteScore(upvotes, downvotes int) int { return (upvotes + 1) - downvotes }

// dedupeVotes keeps the LAST vote for a repeated memory id, preserving
// stable order over the remaining ids (order rarely matters here, but
// deterministic output makes a flaky test easier to root-cause).
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
