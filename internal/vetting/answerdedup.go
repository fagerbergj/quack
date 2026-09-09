// answerdedup.go: collapses a node's chat-visible answer when it only
// restates a record the same round already staged (review, PR body, ...) -
// the fix for the doubled-review-body bug . Structural, not
// prompt-dependent: applies to every agent kind that stages via
// act.stagedDelivery, keyed generically by Kind rather than "review" only.
package vetting

import (
	"fmt"
	"strings"
)

// minDedupBodyLen: below this, a short staged body ("LGTM") is too generic
// to reliably distinguish "the answer restates it" from coincidence.
const minDedupBodyLen = 40

// overlapThreshold: fraction of the staged body's words that must also
// appear in the answer for a paraphrase (not just verbatim containment) to
// count as a restatement.
const overlapThreshold = 0.8

// maxAnswerWordRatio: an answer may carry at most this many times the
// staged body's word count and still count as "just a restatement". Without
// this bound, an answer that quotes the body verbatim (or shares most of its
// words) and then adds a real question, decision, or new finding was being
// collapsed to the one-line status - losing the very content the reply
// exists to carry. Ceiling, not exact: a generous preamble/wrapper still
// passes; substantial added content does not.
const maxAnswerWordRatio = 1.5

// normalizeForCompare lowercases and collapses whitespace so markdown/
// formatting drift between the answer and the staged record doesn't defeat
// the comparison.
func normalizeForCompare(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// restatesRecord reports whether answer substantially repeats body: near-
// verbatim containment either direction, or >= overlapThreshold of body's
// words also present in answer (catches a light paraphrase).
func restatesRecord(answer, body string) bool {
	body = strings.TrimSpace(body)
	if len(body) < minDedupBodyLen {
		return false
	}
	na, nb := normalizeForCompare(answer), normalizeForCompare(body)
	if nb == "" {
		return false
	}
	// answer wholly inside body (a truncation/prefix) carries no extra
	// content by construction - always safe to collapse, no ratio check.
	if strings.Contains(nb, na) {
		return true
	}
	bWords := strings.Fields(nb)
	if len(bWords) == 0 {
		return false
	}
	aWords := strings.Fields(na)
	if float64(len(aWords)) > float64(len(bWords))*maxAnswerWordRatio {
		return false
	}
	if strings.Contains(na, nb) {
		return true
	}
	aSet := make(map[string]struct{}, len(aWords))
	for _, w := range aWords {
		aSet[w] = struct{}{}
	}
	hit := 0
	for _, w := range bWords {
		if _, ok := aSet[w]; ok {
			hit++
		}
	}
	return float64(hit)/float64(len(bWords)) >= overlapThreshold
}

// summarizeStaged renders the one-line status that stands in for an answer
// that only restated this record.
func summarizeStaged(sd StagedDelivery) string {
	switch sd.Kind {
	case "review":
		verdict := sd.Event
		if verdict == "" {
			verdict = "comment"
		}
		n := len(sd.Comments)
		plural := "s"
		if n == 1 {
			plural = ""
		}
		return fmt.Sprintf("Staged: %s (%d inline comment%s).", verdict, n, plural)
	case "pull_request":
		title := sd.Title
		if title == "" {
			title = "(untitled)"
		}
		return "Staged: " + title
	default:
		return "Staged for delivery."
	}
}

// dedupeAnswerAgainstStaged returns answer unchanged unless it substantially
// restates a record already staged this round, in which case it returns a
// short status line derived from that record instead. Recovered entries are
// skipped - their Body was parsed OUT of the answer (no separate staged
// record to be duplicating), and empty bodies never trigger anything.
// Applies to any staged Kind (review, pull_request, ...), not review-only.
func dedupeAnswerAgainstStaged(answer string, staged map[string]StagedDelivery) string {
	// sortedStagedDelivery, not a raw map range: if the answer happens to
	// restate more than one staged record, which one wins the collapse must
	// not depend on Go's randomized map iteration order.
	for _, sd := range sortedStagedDelivery(staged) {
		if sd.Recovered || strings.TrimSpace(sd.Body) == "" {
			continue
		}
		if restatesRecord(answer, sd.Body) {
			return summarizeStaged(sd)
		}
	}
	return answer
}
