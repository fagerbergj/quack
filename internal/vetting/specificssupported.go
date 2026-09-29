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
	return verifiedChecks(ctx, answer, act, load, Verifier{LLM: cfg.JudgeModel, Memo: memo})
}

// judgeEvidenceChars bounds the CITED EVIDENCE section; judgeExcerptChars one claim's excerpt.
const (
	judgeEvidenceChars = 16_000
	judgeExcerptChars  = 500
)

const judgeEvidenceHeader = "CITED EVIDENCE - each cited specific in the deliverable, the code verifier's reading of it, and an excerpt of the cited page (or the search snippet) around it, lower-cased with numbers written as digits. " +
	"Check citation wording and quotes against these excerpts; read a source page with read_artifact only when an excerpt cannot settle a finding - source reads are capped at " +
	"%d per round, %d characters each. A specific marked unsupported already fails the code-owned specifics_supported; do not re-score it.\n"

// judgeEvidenceSection renders runVerify's checks for the judge prompt; "" when none is cited.
func judgeEvidenceSection(checks []UnitCheck) string {
	cited := 0
	for _, c := range checks {
		if c.State != "uncited" {
			cited++
		}
	}
	if cited == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, judgeEvidenceHeader, judgeSourceReadBudget, judgeSourceReadCap)
	shown := 0
	for _, c := range checks {
		if c.State == "uncited" {
			continue
		}
		entry := evidenceEntry(c)
		if b.Len()+len(entry) > judgeEvidenceChars {
			fmt.Fprintf(&b, "(%d more cited specifics not shown)\n", cited-shown)
			break
		}
		b.WriteString(entry)
		shown++
	}
	return b.String()
}

// evidenceEntry: one cited specific, its verifier result and its excerpt.
func evidenceEntry(c UnitCheck) string {
	result := c.Verdict.State
	switch {
	case c.State == "no_stored_text":
		result = "not read: no stored text for this source"
	case result == "":
		result = "not read"
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
