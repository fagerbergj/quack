package memory

import (
	"regexp"
	"strings"
	"time"
)

// duplicateOfRe extracts a survivor id from a DELETE op's reason shaped like
// the consolidation prompts' own convention ("duplicate of <id>") - epic
// #1255 P5 treats only this shape as a lineage-recording absorption; any
// other DELETE reason (e.g. "contradicted by newer info") is a bare
// invalidation, unchanged from before.
var duplicateOfRe = regexp.MustCompile(`(?i)duplicate of[:\s]+['"]?([A-Za-z0-9_-]+)['"]?`)

// parseSurvivorID returns the id a DELETE reason names as the memory it
// duplicates, "" if the reason doesn't match that shape.
func parseSurvivorID(reason string) string {
	m := duplicateOfRe.FindStringSubmatch(reason)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// absorbedByReason is the fixed invalidation_reason an absorbed memory
// carries (epic #1255 P5) - normalized, not the raw "duplicate of" text the
// consolidator wrote, so lineage always reads the same regardless of the
// model's exact wording.
func absorbedByReason(survivorID string) string { return "absorbed by " + survivorID }

// absorbFields is the subset of a memory's state computeAbsorbDelta merges -
// both backends read/write this shape.
type absorbFields struct {
	Upvotes, Downvotes            int
	LastUpvotedAt, LastRecalledAt string
	AbsorbedIDs                   []string
}

// absorbDelta is the survivor's new state after folding one absorbed
// memory in.
type absorbDelta struct {
	Upvotes, Downvotes, VoteScore int
	Tier                          string
	LastUpvotedAt, LastRecalledAt string
	AbsorbedIDs                   []string
}

// computeAbsorbDelta sums survivor+absorbed votes, recomputes tier from the
// new total (verified once upvotes >= 1 - same rule as a supported vote),
// takes the later of the two last_upvoted_at/last_recalled_at, and flattens
// lineage: absorbedID plus anything IT had already absorbed (an absorption
// chain, A absorbed by B absorbed by C, lands all of A/B on C) join
// survivor's own absorbed_ids.
func computeAbsorbDelta(survivor, absorbed absorbFields, absorbedID string) absorbDelta {
	up, down := survivor.Upvotes+absorbed.Upvotes, survivor.Downvotes+absorbed.Downvotes
	tier := TierUnverified
	if up >= 1 {
		tier = TierVerified
	}
	return absorbDelta{
		Upvotes: up, Downvotes: down, VoteScore: up - down, Tier: tier,
		LastUpvotedAt:  maxRFC3339(survivor.LastUpvotedAt, absorbed.LastUpvotedAt),
		LastRecalledAt: maxRFC3339(survivor.LastRecalledAt, absorbed.LastRecalledAt),
		AbsorbedIDs:    mergeAbsorbedIDs(survivor.AbsorbedIDs, absorbed.AbsorbedIDs, absorbedID),
	}
}

// mergeAbsorbedIDs unions survivorIDs, absorbedIDs (the id being absorbed
// may itself already carry a chain), and absorbedID, deduped, stable order.
func mergeAbsorbedIDs(survivorIDs, absorbedIDs []string, absorbedID string) []string {
	seen := make(map[string]bool, len(survivorIDs)+len(absorbedIDs)+1)
	var out []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range survivorIDs {
		add(id)
	}
	for _, id := range absorbedIDs {
		add(id)
	}
	add(absorbedID)
	return out
}

// maxRFC3339 returns whichever of a/b parses as the later RFC3339 timestamp;
// an unparsable or empty side loses to the other, "" if both do.
func maxRFC3339(a, b string) string {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	switch {
	case errA != nil && errB != nil:
		return ""
	case errA != nil:
		return b
	case errB != nil:
		return a
	case tb.After(ta):
		return b
	default:
		return a
	}
}

// absorbedIDSep joins/splits the AbsorbedIDs list for storage as one string
// column/payload value (both backends) - ids are uuids, so a plain comma
// never collides.
const absorbedIDSep = ","

func joinIDs(ids []string) string { return strings.Join(ids, absorbedIDSep) }

func splitIDs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, absorbedIDSep)
}
