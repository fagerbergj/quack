package vetting

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// specifics_cited: code-owned share of a deliverable's substantive specifics
// that sit next to a citation or appear in the research the node received.
const (
	specificsCitedCriterion = "specifics_cited"
	specificsCitedMinCount  = 10 // below this a ratio says nothing; the criterion is absent
	specificsCitedEvidence  = 8  // uncited specifics quoted back to the worker
)

// deliverableText is the answer plus every artifact the worker wrote this
// round: a pointer answer ("see artifact X") is judged on the artifact, so code must be too.
func deliverableText(ctx context.Context, answer string, act workerActivity, load PageLoader) string {
	if load == nil {
		return answer
	}
	var sb strings.Builder
	sb.WriteString(answer)
	for _, id := range act.artifactsWritten {
		body, _, ok, err := load.Latest(ctx, id)
		if err != nil || !ok || !utf8.Valid(body) {
			continue
		}
		sb.WriteString("\n\n")
		sb.Write(body)
	}
	return sb.String()
}

// substantive: a specific worth a citation. Small integers ("5 bench", "week 4")
// and quoted phrases are left out - quotes are the verifier tier's job.
func substantive(s Specific) bool {
	switch s.Kind {
	case "percent", "currency", "date":
		return true
	case "number":
		return strings.Contains(s.Norm, ".") || len(strings.Trim(s.Norm, "-.")) >= 3
	}
	return false
}

// specificsCitedScore counts substantive specifics and how many are backed:
// cited in their unit, or located in the received research (fan-in nodes
// inherit their dependencies' citations, so an uncited figure copied from upstream is grounded).
func specificsCitedScore(units []Unit, received string) (backed, total int, uncited []UnitCheck) {
	for _, u := range units {
		for _, s := range u.Specifics {
			if !substantive(s) {
				continue
			}
			total++
			if len(u.Citations) > 0 {
				backed++
				continue
			}
			if _, found := LocateSpecific(received, s); found {
				backed++
				continue
			}
			uncited = append(uncited, UnitCheck{Unit: u, Specific: s, State: "uncited"})
		}
	}
	return backed, total, uncited
}

// specificsCitedCriterionScore: applicable only when the node's rubric declares
// specifics_cited deterministic, and only once the deliverable carries enough specifics for a ratio to mean anything.
func specificsCitedCriterionScore(ctx context.Context, answer string, act workerActivity, cfg Config, load PageLoader) (criterionScore, bool) {
	if spec, ok := cfg.RubricSpecs[specificsCitedCriterion]; !ok || !spec.Deterministic {
		return criterionScore{}, false
	}
	units := FindUnits(deliverableText(ctx, answer, act, load))
	backed, total, uncited := specificsCitedScore(units, cfg.Task+"\n"+cfg.UpstreamAnswers)
	if total < specificsCitedMinCount {
		return criterionScore{}, false
	}
	score := float64(backed) / float64(total)
	c := criterionScore{Score: score, Reason: fmt.Sprintf(
		"deterministic: %d of %d substantive specifics (figures, percentages, prices, dates) carry a citation in their sentence or block, or appear in the research received", backed, total)}
	for i, u := range uncited {
		if i == specificsCitedEvidence {
			c.Reason += fmt.Sprintf("; %d more", len(uncited)-i)
			break
		}
		c.Evidence = append(c.Evidence, evidenceItem{Ref: u.Specific.Value, Score: 0})
		c.Reason += fmt.Sprintf("; uncited %q in %q", u.Specific.Value, clipRunes(u.Unit.Text, 80))
	}
	return c, true
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}
