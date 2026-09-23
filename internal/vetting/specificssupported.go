package vetting

import (
	"context"
	"fmt"
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
		return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d specifics read against their cited pages, none contradicted", supported)}, true
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
	checks := CheckUnits(ctx, FindUnits(deliverableText(ctx, answer, act, load)), WebPageEvidence{Store: load, Snippets: act.seen})
	return v.VerifyChecks(ctx, checks)
}

// startVerify runs the verify tier beside the judge: it reads no judge output, so
// it adds no wall time, and it runs inside the judge's admission slot. The returned func joins it.
func startVerify(ctx context.Context, cfg Config, answer string, act workerActivity) func() (criterionScore, bool) {
	load := recordReader(cfg)
	if !declaresCodeOwned(cfg, specificsSupportedCriterion) || load == nil || cfg.JudgeModel == nil {
		return func() (criterionScore, bool) { return criterionScore{}, false }
	}
	done := make(chan struct{})
	var c criterionScore
	var ok bool
	go func() {
		defer close(done)
		c, ok = specificsSupportedScore(verifiedChecks(ctx, answer, act, load, Verifier{LLM: cfg.JudgeModel}))
	}()
	return func() (criterionScore, bool) {
		<-done
		return c, ok
	}
}
