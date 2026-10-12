package sleeper

import (
	"slices"
	"sort"

	"github.com/fagerbergj/quack-extensions/sleeper/sleepergen"
)

var flexKindEligible = map[string][]string{
	"WRRB_FLEX":  {"RB", "WR"},
	"REC_FLEX":   {"WR", "TE"},
	"FLEX":       {"RB", "WR", "TE"},
	"SUPER_FLEX": {"QB", "RB", "WR", "TE"},
}

// lineupAssignment is the total and the player ("" if none) filling each nonBenchSlots slot, same order.
type lineupAssignment struct {
	total  float32
	bySlot []string
}

// bestLineup fills strict slots with the top scorer in slot order, then solves every flex slot jointly
// as one exact max-weight assignment.
func bestLineup(rosterPositions []string, playerIDs []string, points map[string]float32, dump map[string]sleepergen.Player) lineupAssignment {
	slots := nonBenchSlots(rosterPositions)
	// Seed every candidate as leftover: a position with no strict slot (TE in a QB/RB/WR/FLEX league)
	// must still reach the flex solver.
	leftover := map[string][]string{}
	for pos, ids := range groupByPosition(playerIDs, dump) {
		leftover[pos] = sortedByPoints(ids, points)
	}
	bySlot := make([]string, len(slots))
	var total float32
	var flexIdx []int
	for i, pos := range slots {
		if flexKindEligible[pos] != nil {
			flexIdx = append(flexIdx, i)
			continue
		}
		group := leftover[pos]
		if len(group) == 0 {
			continue
		}
		bySlot[i], leftover[pos] = group[0], group[1:]
		total += points[group[0]]
	}
	flexTotal, flexAssign := assignFlexSlots(flexSlotKinds(slots, flexIdx), leftover, points)
	for j, idx := range flexIdx {
		bySlot[idx] = flexAssign[j]
	}
	return lineupAssignment{total: total + flexTotal, bySlot: bySlot}
}

func flexSlotKinds(slots []string, flexIdx []int) []string {
	out := make([]string, len(flexIdx))
	for j, idx := range flexIdx {
		out[j] = slots[idx]
	}
	return out
}

// flexCandidate is one leftover player eligible for at least one flex slot.
type flexCandidate struct {
	id  string
	pos string
}

// assignFlexSlots returns the candidate ("" if unfilled) for each slotKinds index.
func assignFlexSlots(slotKinds []string, leftover map[string][]string, points map[string]float32) (float32, []string) {
	if len(slotKinds) == 0 {
		return 0, nil
	}
	posSet := map[string]bool{}
	for _, kind := range slotKinds {
		for _, pos := range flexKindEligible[kind] {
			posSet[pos] = true
		}
	}
	var candidates []flexCandidate
	for pos := range posSet {
		for _, id := range leftover[pos] {
			candidates = append(candidates, flexCandidate{id: id, pos: pos})
		}
	}
	return maxWeightAssignment(slotKinds, candidates, points)
}

// maxWeightAssignment bitmask-DPs the best assignment of distinct candidates to slotKinds, memoizing the
// choice per state to backtrack it.
func maxWeightAssignment(slotKinds []string, candidates []flexCandidate, points map[string]float32) (float32, []string) {
	memo := map[uint64]float32{}
	choice := map[uint64]int{}
	var solve func(slotIdx int, used uint32) float32
	solve = func(slotIdx int, used uint32) float32 {
		if slotIdx == len(slotKinds) {
			return 0
		}
		key := uint64(slotIdx)<<32 | uint64(used)
		if v, ok := memo[key]; ok {
			return v
		}
		best, bestChoice := solve(slotIdx+1, used), -1 // leave this slot unfilled
		for i, c := range candidates {
			bit := uint32(1) << uint(i)
			if used&bit != 0 || !flexEligible(slotKinds[slotIdx], c.pos) {
				continue
			}
			if v := points[c.id] + solve(slotIdx+1, used|bit); v > best {
				best, bestChoice = v, i
			}
		}
		memo[key], choice[key] = best, bestChoice
		return best
	}
	total := solve(0, 0)
	assignment := make([]string, len(slotKinds))
	used := uint32(0)
	for slotIdx := range slotKinds {
		c := choice[uint64(slotIdx)<<32|uint64(used)]
		if c < 0 {
			continue
		}
		assignment[slotIdx] = candidates[c].id
		used |= 1 << uint(c)
	}
	return total, assignment
}

func flexEligible(kind, pos string) bool { return slices.Contains(flexKindEligible[kind], pos) }

func sortedByPoints(ids []string, points map[string]float32) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool { return points[out[i]] > points[out[j]] })
	return out
}

func groupByPosition(playerIDs []string, dump map[string]sleepergen.Player) map[string][]string {
	out := map[string][]string{}
	for _, id := range playerIDs {
		pos := playerPosition(dump, id)
		out[pos] = append(out[pos], id)
	}
	return out
}
