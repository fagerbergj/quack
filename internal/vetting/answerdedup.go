// answerdedup.go: collapses a node's chat answer when it only restates a record the same round already
// staged (review, PR body, ...), keyed by Kind for every agent.
package vetting

import (
	"fmt"
	"strings"
)

// minDedupBodyLen: below this, a short staged body ("LGTM") is too generic
// to reliably distinguish "the answer restates it" from coincidence.
const minDedupBodyLen = 40

// overlapThreshold: fraction of the staged body's words the answer must share for a paraphrase to count
// as a restatement.
const overlapThreshold = 0.8

// maxAnswerWordRatio caps answer/body word count for a restatement: past it, the answer carries real
// added content (a question, decision, finding) that collapsing would lose.
const maxAnswerWordRatio = 1.5

// normalizeForCompare lowercases and collapses whitespace so formatting drift doesn't defeat the comparison.
func normalizeForCompare(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// restatesRecord: near-verbatim containment either way, or >= overlapThreshold of body's words in answer.
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

// dedupeAnswerAgainstStaged swaps answer for a one-line status when it restates a staged record.
// Recovered entries are skipped: their Body was parsed out of the answer itself.
func dedupeAnswerAgainstStaged(answer string, staged map[string]StagedDelivery) string {
	// Sorted, not a raw map range: when the answer restates several records, the winner must be deterministic.
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
