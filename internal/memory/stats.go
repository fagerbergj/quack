package memory

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ScopeStats is one bucket's live/invalidated point counts (epic P5) -
// the memory-page header's "live points per scope over time" secondary
// metric, snapshotted at call time (not itself a weekly series).
type ScopeStats struct {
	Scope       string
	Live        int
	Invalidated int
}

// Snapshot walks every point once (paged, includeInvalidated=true) and
// returns per-scope live/invalidated tallies plus absorbedBy: every
// absorbed id currently listed in some memory's AbsorbedIDs, mapped to that
// memory's id - the input FoldAbsorption and stats weekly folding need to
// attribute a since-merged memory's history to its survivor.
func (s *Store) Snapshot(ctx context.Context) ([]ScopeStats, map[string]string, error) {
	byScope := map[string]*ScopeStats{}
	absorbedBy := map[string]string{}
	err := s.forEachSweepPage(ctx, true, false, func(page []scored) {
		for _, p := range page {
			st, ok := byScope[p.Scope]
			if !ok {
				st = &ScopeStats{Scope: p.Scope}
				byScope[p.Scope] = st
			}
			if p.Status == string(StatusInvalidated) {
				st.Invalidated++
			} else {
				st.Live++
			}
			for _, absorbed := range p.AbsorbedIDs {
				absorbedBy[absorbed] = p.ID
			}
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("memory: stats snapshot: %w", err)
	}
	scopes := make([]ScopeStats, 0, len(byScope))
	for _, st := range byScope {
		scopes = append(scopes, *st)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Scope < scopes[j].Scope })
	return scopes, absorbedBy, nil
}

// VoteEvent/RecallEvent/OpEvent are the minimal ledger/memory_ops facts
// ComputeStats buckets by week - kept free of ledger/store types so this
// package's only ledger dependency stays the one already in preload.go.
type VoteEvent struct {
	MemoryID string
	Vote     string // supported | contradicted | not_relevant
	At       time.Time
}

type RecallEvent struct {
	At time.Time
}

// OpEvent is one memory_ops row's op+timestamp - only "add"/"invalidate" feed
// WeekStats.Minted/Invalidated; other ops (update, vote, reinforce) are
// ignored here (voting is already covered by VoteEvent).
type OpEvent struct {
	Op string
	At time.Time
}

// WeekStats is one ISO week's memory-usage numbers (epic P5).
type WeekStats struct {
	Week         string // ISO 8601 week, e.g. "2026-W23"
	Recalls      int
	Supported    int
	Contradicted int
	NotRelevant  int
	// Precision = Supported / (Supported+Contradicted): of the recalls the judge
	// ruled on, how often the memory was right. NotRelevant is noise, not a
	// wrong memory, so it is excluded. 0 if no such votes.
	Precision float64
	// SupportShare = Supported / Recalls: how much of what was delivered actually
	// helped; unvoted and not-relevant recalls count as no help. 0 if no recalls.
	SupportShare float64
	Minted       int
	Invalidated  int
}

// isoWeekKey formats t (UTC) as its ISO 8601 week.
func isoWeekKey(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// ComputeStats buckets votes/recalls/ops into the `weeks` ISO weeks (UTC)
// ending on now's week, oldest first - a week with no activity still
// appears, zeroed, so a caller can chart a continuous series. A vote/recall
// counts once per week regardless of which memory id it names, so a
// same-week consolidation merge doesn't change the total; per-memory
// attribution across a merge is fold.Result.FoldAbsorption's job, not this
// aggregate view's.
func ComputeStats(now time.Time, weeks int, votes []VoteEvent, recalls []RecallEvent, ops []OpEvent) []WeekStats {
	if weeks <= 0 {
		weeks = 1
	}
	order := make([]string, weeks)
	index := make(map[string]int, weeks)
	for i := 0; i < weeks; i++ {
		key := isoWeekKey(now.AddDate(0, 0, -7*(weeks-1-i)))
		order[i] = key
		index[key] = i
	}
	out := make([]WeekStats, weeks)
	for i, key := range order {
		out[i] = WeekStats{Week: key}
	}
	inRange := func(t time.Time) (int, bool) {
		i, ok := index[isoWeekKey(t)]
		return i, ok
	}
	for _, v := range votes {
		i, ok := inRange(v.At)
		if !ok {
			continue
		}
		// absorbedBy doesn't change a WEEKLY total: a vote counts once
		// regardless of which memory id it names. Per-memory attribution
		// across a merge is fold.Result.FoldAbsorption's job, not this one.
		switch v.Vote {
		case VoteSupported:
			out[i].Supported++
		case VoteContradicted:
			out[i].Contradicted++
		case VoteNotRelevant:
			out[i].NotRelevant++
		}
	}
	for _, r := range recalls {
		if i, ok := inRange(r.At); ok {
			out[i].Recalls++
		}
	}
	for _, o := range ops {
		i, ok := inRange(o.At)
		if !ok {
			continue
		}
		switch OpsLogOp(o.Op) {
		case OpAdd:
			out[i].Minted++
		case OpInvalidate:
			out[i].Invalidated++
		}
	}
	for i := range out {
		if ruled := out[i].Supported + out[i].Contradicted; ruled > 0 {
			out[i].Precision = float64(out[i].Supported) / float64(ruled)
		}
		if out[i].Recalls > 0 {
			out[i].SupportShare = float64(out[i].Supported) / float64(out[i].Recalls)
		}
	}
	return out
}
