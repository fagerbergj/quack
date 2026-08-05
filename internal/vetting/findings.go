// Per-finding verification: staged findings surfaced as numbered claims the judge independently checks.
package vetting

import (
	"fmt"
	"strings"
)

// Per-finding verification outcomes. Only contradicted carries a scoring consequence.
const (
	findingVerified     = "verified"
	findingUnsupported  = "unsupported"
	findingContradicted = "contradicted"
)

// findingVerdict: one staged finding's verification. Index is 1-based for pairing.
type findingVerdict struct {
	Index  int    `json:"index" jsonschema:"the finding's 1-based position in the numbered list you were given"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Status string `json:"status" jsonschema:"verified (the code backs this claim), unsupported (you could not confirm it either way), or contradicted (the code shows the opposite of the claim)"`
	Why    string `json:"why,omitempty" jsonschema:"one or two sentences citing what you actually found when you checked"`
}

// judgeFindingsInstructions: tells the judge to verify each claim, reading beyond the cited line for context.
const judgeFindingsInstructions = "STAGED FINDINGS TO VERIFY - each numbered item below is a specific, checkable claim the review makes about this code. Before scoring claims_grounded, independently verify EACH ONE against the repo using your read tools: read the cited path/line AND whatever else you need to judge it in context - a caller, an interface, a test, a related file. Do not stop at the cited line and do not take the finding's word for it; a finding can be locally accurate and wrong in context, or refutable only by a file it never mentions. For EVERY finding, record a result in submit_verdict's `findings` array: {index, path, line, status, why}, where status is `verified` (the code backs the claim), `unsupported` (you could not confirm it either way), or `contradicted` (the code shows the opposite of the claim - use this only when the code plainly disagrees, not for a stylistic difference of opinion; a contradicted finding automatically fails claims_grounded).\n\n"

// stagedFindingsSection renders the reviewer's staged findings as a numbered list for the judge.
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

// findingsGroundingCriterion: claims_grounded - sunk by a contradicted finding, same override pattern as cites_sources.
const findingsGroundingCriterion = "claims_grounded"

// applyFindingsVerdict sinks findingsGroundingCriterion to 0 when any staged finding is contradicted.
// verified/unsupported findings never move the score - they only feed composeFindingsFeedback.
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

// findingLoc formats a finding's location for feedback, tolerating a missing line.
func findingLoc(f findingVerdict) string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
	return f.Path
}

// composeFindingsFeedback renders the judge's per-finding verdicts as revise feedback.
// Silent on verified findings. "" when nothing to report.
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
