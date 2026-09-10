package memory

import (
	"math"
	"testing"
	"time"
)

// TestComputeStats_WeeklyPrecision seeds two ISO weeks (UTC) of votes and checks precision/
// support-share and week-boundary attribution: a vote at 23:59:59 Saturday UTC
// (mid ISO week) and one 24h later (the next ISO week, Sunday->Monday crossing) land in different buckets.
func TestComputeStats_WeeklyPrecision(t *testing.T) {
	// 2026-01-05 is a Monday (ISO week 2026-W02).
	week1Mon := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	week1Sun := time.Date(2026, 1, 11, 23, 59, 59, 0, time.UTC) // still W02
	week2Mon := time.Date(2026, 1, 12, 0, 0, 1, 0, time.UTC)    // W03

	votes := []VoteEvent{
		{MemoryID: "a", Vote: VoteSupported, At: week1Mon},
		{MemoryID: "b", Vote: VoteSupported, At: week1Sun},
		{MemoryID: "c", Vote: VoteContradicted, At: week1Sun},
		{MemoryID: "d", Vote: VoteNotRelevant, At: week1Sun},
		{MemoryID: "e", Vote: VoteSupported, At: week2Mon},
	}
	recalls := []RecallEvent{{At: week1Mon}, {At: week1Mon}, {At: week1Sun}, {At: week1Sun}, {At: week2Mon}}
	ops := []OpEvent{
		{Op: string(OpAdd), At: week1Mon},
		{Op: string(OpInvalidate), At: week1Sun},
		{Op: string(OpVote), At: week1Sun}, // not minted/invalidated - must not count
	}

	now := week2Mon
	weeks := ComputeStats(now, 2, votes, recalls, ops)
	if len(weeks) != 2 {
		t.Fatalf("len(weeks) = %d, want 2", len(weeks))
	}
	w1, w2 := weeks[0], weeks[1]

	if w1.Week != "2026-W02" || w2.Week != "2026-W03" {
		t.Fatalf("week keys = %q, %q, want 2026-W02, 2026-W03", w1.Week, w2.Week)
	}
	if w1.Supported != 2 || w1.Contradicted != 1 || w1.NotRelevant != 1 {
		t.Fatalf("W02 votes = %+v, want supported=2 contradicted=1 not_relevant=1", w1)
	}
	// precision = supported / (supported+contradicted+not_relevant) = 2/4 = 0.5
	// 2 supported of 3 ruled on (not_relevant excluded); 2 supported of 4 delivered.
	if math.Abs(w1.Precision-2.0/3.0) > 1e-9 || w1.SupportShare != 0.5 {
		t.Fatalf("W02 precision=%v support_share=%v, want 0.667/0.5", w1.Precision, w1.SupportShare)
	}
	if w1.Recalls != 4 {
		t.Fatalf("W02 recalls = %d, want 4", w1.Recalls)
	}
	if w1.Minted != 1 || w1.Invalidated != 1 {
		t.Fatalf("W02 minted=%d invalidated=%d, want 1/1 (the vote op must not count as either)", w1.Minted, w1.Invalidated)
	}

	if w2.Supported != 1 || w2.Precision != 1 {
		t.Fatalf("W03 = %+v, want supported=1 precision=1 (the Sunday->Monday vote must not leak into W02)", w2)
	}
	if w2.Recalls != 1 {
		t.Fatalf("W03 recalls = %d, want 1", w2.Recalls)
	}
}

// TestComputeStats_EmptyWeekIsZeroed proves a week with no activity still
// appears (a caller can chart a continuous series), not just skipped.
func TestComputeStats_EmptyWeekIsZeroed(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	weeks := ComputeStats(now, 3, nil, nil, nil)
	if len(weeks) != 3 {
		t.Fatalf("len(weeks) = %d, want 3", len(weeks))
	}
	for _, w := range weeks {
		if w.Recalls != 0 || w.Supported != 0 || w.Precision != 0 {
			t.Fatalf("empty week %+v should be all-zero", w)
		}
	}
}
