package vetting

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// IsCodeOwnedCriterion reports whether a deterministic check, not the judge, scores name.
func IsCodeOwnedCriterion(name string) bool {
	return slices.Contains(CodeOwnedCriteria(), name)
}

// LintRubric reports every authoring problem in one rubric.yaml beyond what boot rejects. Unknown keys fail
// since a rubric has no weights (aggregation is weakest-link); lintCriterion holds the per-criterion checks.
func LintRubric(raw []byte, source string) []string {
	var doc rubricDoc
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return []string{fmt.Sprintf("%s: %v", source, err)}
	}
	var issues []string
	if err := validateRubricDoc(doc); err != nil {
		issues = append(issues, fmt.Sprintf("%s: %v", source, err))
	}
	byDefinition := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(doc.Criteria)) {
		c, scale := doc.Criteria[name], doc.Scale
		if c.Scale != nil {
			scale = *c.Scale
		}
		for _, problem := range lintCriterion(name, c, scale, byDefinition) {
			issues = append(issues, fmt.Sprintf("%s: criterion %q: %s", source, name, problem))
		}
	}
	return issues
}

// lintCriterion is LintRubric's per-criterion checks; byDefinition collects definitions across calls.
func lintCriterion(name string, c rubricCriterion, scale rubricScale, byDefinition map[string]string) []string {
	var problems []string
	def := strings.Join(strings.Fields(strings.ToLower(c.Definition)), " ")
	switch {
	case def == "":
		problems = append(problems, "no definition")
	case byDefinition[def] != "":
		problems = append(problems, fmt.Sprintf("same definition as %q", byDefinition[def]))
	default:
		byDefinition[def] = name
	}
	owned := IsCodeOwnedCriterion(name)
	if c.Deterministic {
		if !owned {
			problems = append(problems, "deterministic, but no check scores a criterion of this name")
		}
		return problems
	}
	if owned {
		problems = append(problems, "a code check owns this name, so the judge's score is dropped; mark it deterministic: true")
	}
	if len(c.Bands) == 0 {
		problems = append(problems, "no bands, so the judge has no pass bar to score against")
	}
	if scale.Min != 0 || scale.Max != judgeScaleMax {
		problems = append(problems, fmt.Sprintf("judged on scale [%v, %v], but the judge prompt asks for an integer 0-%d", scale.Min, scale.Max, judgeScaleMax))
	}
	return problems
}
