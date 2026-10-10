package memory

import (
	"regexp"
	"strings"
	"time"
)

// duplicateOfRe extracts a survivor id from a "duplicate of <id>" DELETE reason; only that shape
// records lineage, any other reason is a bare invalidation.
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

// absorbedByReason is the normalized invalidation_reason of an absorbed memory, independent of
// the model's wording.
func absorbedByReason(survivorID string) string { return "absorbed by " + survivorID }

// absorbFields is the subset of a memory's state computeAbsorbDelta merges -
// both backends read/write this shape.
type absorbFields struct {
	Upvotes, Downvotes, Supported, NotRelevant int
	LastUpvotedAt, LastRecalledAt              string
	AbsorbedIDs                                []string
}

type absorbDelta struct {
	Upvotes, Downvotes, Supported, NotRelevant, VoteScore int
	Tier                                                  string
	LastUpvotedAt, LastRecalledAt                         string
	AbsorbedIDs                                           []string
}

// computeAbsorbDelta sums both memories' votes, recomputes tier, keeps the later timestamps, and flattens
// lineage so an absorption chain (A into B into C) lands A and B on C.
func computeAbsorbDelta(survivor, absorbed absorbFields, absorbedID string) absorbDelta {
	up, down := survivor.Upvotes+absorbed.Upvotes, survivor.Downvotes+absorbed.Downvotes
	supported, notRelevant := survivor.Supported+absorbed.Supported, survivor.NotRelevant+absorbed.NotRelevant
	return absorbDelta{
		Upvotes: up, Downvotes: down, Supported: supported, NotRelevant: notRelevant,
		VoteScore: up - down, Tier: tierFromSupported(supported),
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

// absorbedIDSep joins AbsorbedIDs into one stored value; ids are uuids, so a comma never collides.
const absorbedIDSep = ","

func joinIDs(ids []string) string { return strings.Join(ids, absorbedIDSep) }

func splitIDs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, absorbedIDSep)
}
