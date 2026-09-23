package vetting

import "testing"

// TestJudgeScoreForDeterministicCriterionIsDropped: when code had nothing to
// compute for a code-owned criterion, the judge's own score for it must not stand in.
func TestJudgeScoreForDeterministicCriterionIsDropped(t *testing.T) {
	specs := map[string]criterionSpec{
		"cites_sources": {Name: "cites_sources", Deterministic: true},
		"grounded":      {Name: "grounded", Definition: "g"},
	}
	v := verdict{Criteria: map[string]criterionScore{
		"cites_sources": {Score: 0.3},
		"grounded":      {Score: 0.9},
	}}
	v = applyRubricSpecs(v, specs)
	if _, ok := v.Criteria["cites_sources"]; ok {
		t.Errorf("judge-scored cites_sources survived applyRubricSpecs: %+v", v.Criteria)
	}
	if v.Criteria["grounded"].Definition != "g" {
		t.Errorf("grounded spec not applied: %+v", v.Criteria["grounded"])
	}
	det := verdict{Criteria: map[string]criterionScore{"cites_sources": {Score: 0.3, Deterministic: true}}}
	if _, ok := applyRubricSpecs(det, specs).Criteria["cites_sources"]; !ok {
		t.Error("the code-computed cites_sources score must be kept")
	}
}
