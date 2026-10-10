package memory

import (
	"context"
	"fmt"
)

// Mutations are read-modify-write over index.getMany/patch with no isolation: sqlite autocommits each
// statement, qdrant waits on each SetPayload. ponytail: concurrent votes on one memory can lose one.

// getByID fetches one point by id regardless of status - the caller decides what invalidated means.
func (s *Store) getByID(ctx context.Context, id string) (scored, bool, error) {
	got, err := s.idx.getMany(ctx, []string{id})
	if err != nil {
		return scored{}, false, err
	}
	p, ok := got[id]
	return p, ok, nil
}

// getOrdered returns ids' existing points deduped, in first-seen order of ids.
func (s *Store) getOrdered(ctx context.Context, ids []string) ([]scored, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	got, err := s.idx.getMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]scored, 0, len(got))
	for _, id := range ids {
		if p, ok := got[id]; ok {
			out = append(out, p)
			delete(got, id)
		}
	}
	return out, nil
}

func invalidatedSet(ts, reason string) map[string]any {
	return map[string]any{payloadStatus: string(StatusInvalidated), payloadInvalidatedAt: ts, payloadInvalidationReason: reason}
}

func isInvalidated(p scored) bool { return p.Status == string(StatusInvalidated) }

// invalidateByID soft-invalidates ids (re-stamping already-invalidated ones) and reports how many existed.
func (s *Store) invalidateByID(ctx context.Context, ids []string, reason string) (int, error) {
	pts, err := s.getOrdered(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("memory: get before invalidate: %w", err)
	}
	if len(pts) == 0 {
		return 0, nil
	}
	if err := s.idx.patch(ctx, idsOf(pts), invalidatedSet(nowRFC3339(), reason)); err != nil {
		return 0, fmt.Errorf("memory: invalidate: %w", err)
	}
	return len(pts), nil
}

// updateStatus applies o to every non-invalidated id and returns those touched; invalidation skips
// verified ones, since a verified memory recalled into a closed-unmerged chat gets no vote.
func (s *Store) updateStatus(ctx context.Context, ids []string, o OutcomeSignal) ([]string, error) {
	pts, err := s.getOrdered(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("memory: get for outcome: %w", err)
	}
	var cands []scored
	for _, p := range pts {
		if isInvalidated(p) || (o.Kind == OutcomeInvalidated && p.Tier == TierVerified) {
			continue
		}
		cands = append(cands, p)
	}
	if len(cands) == 0 {
		return nil, nil
	}
	ts := nowRFC3339()
	if o.Kind == OutcomeInvalidated {
		if err := s.idx.patch(ctx, idsOf(cands), invalidatedSet(ts, o.Reason)); err != nil {
			return nil, fmt.Errorf("memory: outcome invalidate: %w", err)
		}
		return idsOf(cands), nil
	}
	for _, p := range cands { // reinforce never writes tier: tier tracks judge/human support only
		if err := s.idx.patch(ctx, []string{p.ID}, map[string]any{
			payloadStatus: string(StatusReinforced), payloadReinforcementCount: p.ReinforcementCount + 1,
			payloadUpvotes: p.Upvotes + 1, payloadVoteScore: reinforcedVoteScore(p.Upvotes, p.Downvotes), payloadLastUpvotedAt: ts,
		}); err != nil {
			return nil, fmt.Errorf("memory: outcome reinforce: %w", err)
		}
	}
	return idsOf(cands), nil
}

// demoteTier sets tier=unverified on every verified id and returns only those it changed,
// so the caller logs no memory_ops row for an already-unverified id.
func (s *Store) demoteTier(ctx context.Context, ids []string) ([]string, error) {
	pts, err := s.getOrdered(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("memory: get for demote: %w", err)
	}
	var touched []string
	for _, p := range pts {
		if p.Tier == TierVerified {
			touched = append(touched, p.ID)
		}
	}
	if len(touched) == 0 {
		return nil, nil
	}
	if err := s.idx.patch(ctx, touched, map[string]any{payloadTier: TierUnverified}); err != nil {
		return nil, fmt.Errorf("memory: demote: %w", err)
	}
	return touched, nil
}

// voteSet is one vote's write; notRelevant is false for a human vote, which never moves that count.
func voteSet(d voteDelta, ts, reason string, notRelevant bool) map[string]any {
	set := map[string]any{
		payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadSupported: d.Supported,
		payloadVoteScore: d.VoteScore, payloadTier: d.Tier,
	}
	if notRelevant {
		set[payloadNotRelevant] = d.NotRelevant
	}
	if d.LastUpvotedAt != "" {
		set[payloadLastUpvotedAt] = d.LastUpvotedAt
	}
	if d.Invalidate {
		for k, v := range invalidatedSet(ts, reason) {
			set[k] = v
		}
	}
	return set
}

// applyVotes applies each vote to a non-invalidated memory and returns the ids touched, in vote order;
// a net score <= invalidateThreshold soft-invalidates it.
func (s *Store) applyVotes(ctx context.Context, votes []Vote, invalidateThreshold int) ([]string, error) {
	if len(votes) == 0 {
		return nil, nil
	}
	ids := make([]string, len(votes))
	for i, v := range votes {
		ids[i] = v.MemoryID
	}
	existing, err := s.idx.getMany(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("memory: get for votes: %w", err)
	}
	ts := nowRFC3339()
	var touched []string
	for _, v := range votes {
		p, ok := existing[v.MemoryID]
		if !ok || isInvalidated(p) {
			continue
		}
		d := computeVoteDelta(p.Upvotes, p.Downvotes, p.Supported, p.NotRelevant, ts, v, invalidateThreshold)
		if err := s.idx.patch(ctx, []string{p.ID}, voteSet(d, ts, d.InvalidateReason, true)); err != nil {
			return nil, fmt.Errorf("memory: vote update: %w", err)
		}
		touched = append(touched, p.ID)
	}
	return touched, nil
}

// setHumanVote sets the caller's vote on id, deriving counts from the transition away from the prior
// human_vote so a toggle never double counts. Reports whether id existed and wasn't invalidated.
func (s *Store) setHumanVote(ctx context.Context, id, vote string, invalidateThreshold int) (bool, error) {
	p, ok, err := s.getByID(ctx, id)
	if err != nil {
		return false, fmt.Errorf("memory: get for human vote: %w", err)
	}
	if !ok || isInvalidated(p) {
		return false, nil
	}
	ts := nowRFC3339()
	d := computeHumanVoteDelta(p.Upvotes, p.Downvotes, p.Supported, p.HumanVote, vote, ts, invalidateThreshold)
	set := voteSet(d, ts, OutcomeReasonNetScore, false)
	set[payloadHumanVote] = vote
	if vote == HumanVoteNone {
		set[payloadHumanVote] = ""
	}
	if err := s.idx.patch(ctx, []string{id}, set); err != nil {
		return false, fmt.Errorf("memory: human vote update: %w", err)
	}
	return true, nil
}

func absorbFieldsOf(p scored) absorbFields {
	return absorbFields{
		Upvotes: p.Upvotes, Downvotes: p.Downvotes, Supported: p.Supported, NotRelevant: p.NotRelevant,
		LastUpvotedAt: p.LastUpvotedAt, LastRecalledAt: p.LastRecalledAt, AbsorbedIDs: p.AbsorbedIDs,
	}
}

// absorb folds absorbedID's votes/timestamps/lineage into survivorID and invalidates absorbedID.
// False (no-op) if either is missing or absorbedID is already invalidated (first invalidation wins).
func (s *Store) absorb(ctx context.Context, survivorID, absorbedID, reason string) (bool, error) {
	got, err := s.idx.getMany(ctx, []string{survivorID, absorbedID})
	if err != nil {
		return false, fmt.Errorf("memory: get for absorb: %w", err)
	}
	sv, ok1 := got[survivorID]
	ab, ok2 := got[absorbedID]
	if !ok1 || !ok2 || isInvalidated(ab) {
		return false, nil
	}
	d := computeAbsorbDelta(absorbFieldsOf(sv), absorbFieldsOf(ab), absorbedID)
	set := map[string]any{
		payloadUpvotes: d.Upvotes, payloadDownvotes: d.Downvotes, payloadSupported: d.Supported, payloadNotRelevant: d.NotRelevant,
		payloadVoteScore: d.VoteScore, payloadTier: d.Tier, payloadAbsorbedIDs: joinIDs(d.AbsorbedIDs),
	}
	if d.LastUpvotedAt != "" {
		set[payloadLastUpvotedAt] = d.LastUpvotedAt
	}
	if d.LastRecalledAt != "" {
		set[payloadLastRecalledAt] = d.LastRecalledAt
	}
	if err := s.idx.patch(ctx, []string{survivorID}, set); err != nil {
		return false, fmt.Errorf("memory: absorb survivor: %w", err)
	}
	if err := s.idx.patch(ctx, []string{absorbedID}, invalidatedSet(nowRFC3339(), reason)); err != nil {
		return false, fmt.Errorf("memory: absorb invalidate: %w", err)
	}
	return true, nil
}

func idsOf(pts []scored) []string {
	ids := make([]string, len(pts))
	for i, p := range pts {
		ids[i] = p.ID
	}
	return ids
}
