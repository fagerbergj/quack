package vetting

import (
	"context"
	"fmt"
	"strings"
)

// specifics_supported: code-owned hard gate over the verify tier. One specific
// its cited page contradicts, on two reads, fails the round (owner decision: zero tolerance).
const specificsSupportedCriterion = "specifics_supported"

// declaresCodeOwned: the node's rubric names criterion as deterministic - the
// switch that turns a code-owned check on for an agent.
func declaresCodeOwned(cfg Config, criterion string) bool {
	spec, ok := cfg.RubricSpecs[criterion]
	return ok && spec.Deterministic
}

// specificsSupportedScore folds verified checks into the criterion; absent when
// nothing got a supported or unsupported verdict (a failed verifier is never a negative).
func specificsSupportedScore(checks []UnitCheck) (criterionScore, bool) {
	supported := 0
	var bad []UnitCheck
	for _, c := range checks {
		switch c.Verdict.State {
		case "supported":
			supported++
		case "unsupported":
			bad = append(bad, c)
		}
	}
	if supported+len(bad) == 0 {
		return criterionScore{}, false
	}
	if len(bad) == 0 {
		return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d specifics read against their cited pages or those pages' search snippets, none contradicted", supported)}, true
	}
	c := criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: %d of %d specifics read against their cited pages are contradicted by them", len(bad), supported+len(bad))}
	for _, b := range bad {
		c.Evidence = append(c.Evidence, evidenceItem{Ref: b.Citation, Score: 0})
		c.Reason += fmt.Sprintf("; %q in %q - the page says %q (%s)", b.Specific.Value, clipRunes(b.Unit.Text, 80), clipRunes(b.Verdict.Quote, 160), b.Citation)
	}
	return c, true
}

// verifiedChecks runs the locate and verify tiers over the deliverable (answer plus
// the artifacts the node wrote), reading pages and artifacts through load.
func verifiedChecks(ctx context.Context, answer string, act workerActivity, load PageLoader, v Verifier) []UnitCheck {
	checks := CheckUnits(ctx, FindUnits(deliverableText(ctx, answer, act, load)), WebPageEvidence{Store: load, Snippets: act.seen, Allowed: act.ownArtifactIDs()})
	return v.VerifyChecks(ctx, checks)
}

// runVerify runs the verify tier ahead of the judge, inside its admission slot, so the
// judge prompt carries its per-claim results; memo spares claims unchanged since an earlier round.
func runVerify(ctx context.Context, cfg Config, answer string, act workerActivity, memo map[string]Verdict) []UnitCheck {
	load := recordReader(cfg)
	if !declaresCodeOwned(cfg, specificsSupportedCriterion) || load == nil || cfg.JudgeModel == nil {
		return nil
	}
	return verifiedChecks(ctx, answer, act, load, Verifier{LLM: cfg.JudgeModel, Memo: memo, TryAdmit: cfg.TryAdmitVerify, ThinkingLevel: cfg.JudgeThinkingLevel})
}

// judgeEvidenceChars bounds the CITED EVIDENCE section; judgeExcerptChars one claim's excerpt.
const (
	judgeEvidenceChars = 16_000
	judgeExcerptChars  = 500
)

const judgeEvidenceHeader = "CITED EVIDENCE - code read the cited specifics against their fetched pages: %d matched. " +
	"The ones below did not, or could not be confirmed; each shows the verifier's reading and an excerpt of the page (or search snippet) around it, " +
	"lower-cased; around a figure, number words appear as digits without thousands separators, and around a quote, curly punctuation and markup are normalized. " +
	"A specific marked unsupported already fails the code-owned specifics_supported; do not re-score it.\n"

// unconfirmed: the verifier read the specific and could not confirm it - the only ones the
// judge is shown, since a matched specific's excerpt only invites re-checking settled wording.
func unconfirmed(c UnitCheck) bool {
	switch c.Verdict.State {
	case "unsupported", "cannot_tell", "not_checked":
		return true
	}
	return false
}

// judgeEvidenceSection renders runVerify's unconfirmed checks for the judge prompt; "" when
// every cited specific matched (or none was read), so a clean answer's prompt is unchanged.
func judgeEvidenceSection(checks []UnitCheck) string {
	matched, problems := 0, 0
	for _, c := range checks {
		switch {
		case c.Verdict.State == "supported":
			matched++
		case unconfirmed(c):
			problems++
		}
	}
	if problems == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, judgeEvidenceHeader, matched)
	shown := 0
	for _, c := range checks {
		if !unconfirmed(c) {
			continue
		}
		entry := evidenceEntry(c)
		if b.Len()+len(entry) > judgeEvidenceChars {
			fmt.Fprintf(&b, "(%d more not shown)\n", problems-shown)
			break
		}
		b.WriteString(entry)
		shown++
	}
	return b.String()
}

// checkedPages: the stored web_pages the verify tier read a cited specific against (snippets excluded).
func checkedPages(checks []UnitCheck) map[string]bool {
	pages := map[string]bool{}
	for _, c := range checks {
		if c.Verdict.State != "" && c.pageID != "" {
			pages[c.pageID] = true
		}
	}
	return pages
}

// evidenceEntry: one cited specific, its verifier result and its excerpt.
func evidenceEntry(c UnitCheck) string {
	result := c.Verdict.State
	switch {
	case c.Verdict.Quote != "":
		result += fmt.Sprintf(" - page says %q", clipRunes(c.Verdict.Quote, 160))
	case c.Verdict.Reason != "":
		result += " - " + c.Verdict.Reason
	}
	out := fmt.Sprintf("- %q in %q (%s)\n  verifier: %s\n", c.Specific.Value, clipRunes(c.Unit.Text, 160), c.Citation, result)
	if c.Window != "" {
		out += fmt.Sprintf("  excerpt: %q\n", centerClip(c.Window, judgeExcerptChars))
	}
	return out
}

// centerClip keeps s's middle n bytes: a located window has its match at the centre.
func centerClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	lo := (len(s) - n) / 2
	return "..." + strings.ToValidUTF8(s[lo:lo+n], "") + "..."
}
