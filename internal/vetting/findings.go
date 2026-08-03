// Per-finding verification (#498 step 2/3): the judge no longer grades a
// review's findings as prose it takes on faith - each staged inline finding
// (ReviewStage/StagedDelivery.Comments) is surfaced as an explicit numbered
// claim, and the judge must independently check each one against the repo
// with its existing read tools before it may pass claims_grounded. This is
// the fix for #494: a precise but false "off-by-one" finding passed with
// claims_grounded=1 because nothing ever verified it against the file.
package vetting

import (
	"fmt"
	"strings"
)

// Per-finding verification outcomes the judge assigns in submit_verdict's
// findings array. contradicted is the only one that carries a scoring
// consequence (applyFindingsVerdict) - verified/unsupported are informational,
// surfaced to the reviewer via composeFindingsFeedback so it can correct or
// stand by them.
const (
	findingVerified     = "verified"
	findingUnsupported  = "unsupported"
	findingContradicted = "contradicted"
)

// findingVerdict is the judge's independent verification of ONE staged review
// finding against the actual repo - not the review's own account of it.
// Index is the finding's 1-based position in stagedFindingsSection's numbered
// list, so the gate can pair a result back to the exact ReviewComment it
// judged without a fuzzy text match.
type findingVerdict struct {
	Index  int    `json:"index" jsonschema:"the finding's 1-based position in the numbered list you were given"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Status string `json:"status" jsonschema:"verified (the code backs this claim), unsupported (you could not confirm it either way), or contradicted (the code shows the opposite of the claim)"`
	Why    string `json:"why,omitempty" jsonschema:"one or two sentences citing what you actually found when you checked"`
}

// judgeFindingsInstructions prefixes the numbered findings list
// (stagedFindingsSection): it tells the judge to verify each claim itself,
// explicitly permits reading beyond the cited line (a finding can be locally
// accurate and wrong in context, or refutable only by a file it never
// mentions - #498's whole point is that the judge's view must NOT be
// narrowed to path:line), and names the findings array's shape.
const judgeFindingsInstructions = "STAGED FINDINGS TO VERIFY - each numbered item below is a specific, checkable claim the review makes about this code. Before scoring claims_grounded, independently verify EACH ONE against the repo using your read tools: read the cited path/line AND whatever else you need to judge it in context - a caller, an interface, a test, a related file. Do not stop at the cited line and do not take the finding's word for it; a finding can be locally accurate and wrong in context, or refutable only by a file it never mentions. For EVERY finding, record a result in submit_verdict's `findings` array: {index, path, line, status, why}, where status is `verified` (the code backs the claim), `unsupported` (you could not confirm it either way), or `contradicted` (the code shows the opposite of the claim - use this only when the code plainly disagrees, not for a stylistic difference of opinion; a contradicted finding automatically fails claims_grounded).\n\n"

// stagedFindingsSection renders the reviewer's staged inline findings
// (act.stagedDelivery["review"].Comments) as an explicit numbered list for
// the judge prompt, instead of leaving them buried in the review's prose
// where the judge could only take their plausibility on faith. "" when
// nothing is staged (a reviewer that hasn't found anything yet, or a
// non-review node).
func stagedFindingsSection(act workerActivity) string {
	sd, ok := act.stagedDelivery["review"]
	if !ok || len(sd.Comments) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(judgeFindingsInstructions)
	for i, c := range sd.Comments {
		fmt.Fprintf(&sb, "%d. %s:%d - %s\n", i+1, c.Path, c.Line, c.Body)
	}
	sb.WriteString("\n")
	return sb.String()
}

// findingsGroundingCriterion is the code-reviewer rubric criterion
// (agents/code-reviewer/rubric.md's claims_grounded) a contradicted staged
// finding sinks - the same override pattern foldDeterministic uses for
// cites_sources: code owns a criterion's score once it holds ground truth
// (here, the judge's own per-finding check) a holistic guess can't outrank.
const findingsGroundingCriterion = "claims_grounded"

// applyFindingsVerdict sinks findingsGroundingCriterion to 0 when ANY staged
// finding was judged "contradicted" - the code shows the opposite of what the
// review claims (#498, the #494 regression). Weakest-link gating then does
// the rest: aggregateVerdict's caller takes the lowest criterion, so this one
// failure sinks the whole verdict without a parallel pass/fail path.
// verified/unsupported findings never move the score here - they only feed
// composeFindingsFeedback. A no-op when there is nothing contradicted (every
// node but a reviewer with a staged, judge-refuted finding).
func applyFindingsVerdict(v *verdict) {
	var bad []findingVerdict
	for _, f := range v.Findings {
		if strings.EqualFold(strings.TrimSpace(f.Status), findingContradicted) {
			bad = append(bad, f)
		}
	}
	if len(bad) == 0 {
		return
	}
	if v.Criteria == nil {
		v.Criteria = map[string]criterionScore{}
	}
	parts := make([]string, 0, len(bad))
	for _, f := range bad {
		parts = append(parts, fmt.Sprintf("finding #%d (%s) is contradicted by the code - %s", f.Index, findingLoc(f), strings.TrimSpace(f.Why)))
	}
	v.Criteria[findingsGroundingCriterion] = criterionScore{
		Score:  0,
		Reason: "judge verification: " + strings.Join(parts, "; "),
	}
}

// findingLoc formats a finding's location for feedback text, tolerating a
// missing line (path-only claim).
func findingLoc(f findingVerdict) string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
	return f.Path
}

// composeFindingsFeedback renders the judge's per-finding verdicts as
// self-contained revise feedback: which finding, at which location, and what
// the judge found when it independently checked - so the reviewer can act
// without re-deriving anything. Silent on `verified` findings (no penalty,
// nothing to fix). Names unstage_review_comment (added for #562, on PR #628 -
// referenced here in TEXT only, per #498's scope) as how the reviewer
// retracts a finding it no longer stands behind. "" when there is nothing to
// report (no findings staged, or all verified).
func composeFindingsFeedback(findings []findingVerdict) string {
	var lines []string
	for _, f := range findings {
		if strings.EqualFold(strings.TrimSpace(f.Status), findingVerified) {
			continue
		}
		lines = append(lines, fmt.Sprintf("- Finding #%d (%s) is %s: %s", f.Index, findingLoc(f), strings.ToUpper(f.Status), strings.TrimSpace(f.Why)))
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Per-finding verification (the judge independently checked each staged finding against the code):\n")
	sb.WriteString(strings.Join(lines, "\n"))
	sb.WriteString("\n\nCorrect or retract any finding above that does not hold. If your review MCP surface offers unstage_review_comment, call it to retract a finding you no longer stand behind; if it doesn't yet, remove or correct the claim in your next revision.")
	return sb.String()
}
