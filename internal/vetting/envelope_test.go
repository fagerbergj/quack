package vetting

import "testing"

// TestJudgeScoreForDeterministicCriterionIsDropped: when code had nothing to
// compute for a code-owned criterion, the judge's own score for it must not
// stand in - neither as a criterion nor inside the aggregate the gate passes on.
func TestJudgeScoreForDeterministicCriterionIsDropped(t *testing.T) {
	specs := map[string]criterionSpec{
		"checks_ok": {Name: "checks_ok", Deterministic: true},
		"grounded":  {Name: "grounded", Definition: "g"},
	}
	v := verdict{Criteria: map[string]criterionScore{
		"checks_ok":     {Score: 0.2}, // declared deterministic by the rubric
		"cites_sources": {Score: 0.3}, // code-owned even without a rubric spec
		"grounded":      {Score: 0.9},
	}}
	v = mergeDeterministic(v, nil, Config{RubricSpecs: specs})
	for _, name := range []string{"checks_ok", "cites_sources"} {
		if _, ok := v.Criteria[name]; ok {
			t.Errorf("judge-scored %s survived mergeDeterministic: %+v", name, v.Criteria)
		}
	}
	if v.Score < 0.89 {
		t.Errorf("aggregate = %.2f still carries the dropped scores", v.Score)
	}
	v = mergeDeterministic(verdict{Criteria: map[string]criterionScore{"cites_sources": {Score: 0.3}}}, map[string]criterionScore{"cites_sources": {Score: 0.3}}, Config{})
	if c, ok := v.Criteria["cites_sources"]; !ok || !c.Deterministic {
		t.Error("the code-computed cites_sources score must be kept")
	}
}
