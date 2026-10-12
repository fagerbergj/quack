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

// codeOwnedCriteria: every criterion name a deterministic check scores, so the only names a rubric
// may mark `deterministic: true`.
func codeOwnedCriteria() map[string]bool {
	out := map[string]bool{"cites_sources": true, specificsCitedCriterion: true, specificsSupportedCriterion: true}
	for name := range deterministicCriterionSpec {
		out[name] = true
	}
	return out
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
	owned := codeOwnedCriteria()
	byDefinition := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(doc.Criteria)) {
		for _, problem := range lintCriterion(name, doc.Criteria[name], owned, byDefinition) {
			issues = append(issues, fmt.Sprintf("%s: criterion %q: %s", source, name, problem))
		}
	}
	return issues
}

// lintCriterion is LintRubric's per-criterion checks; byDefinition collects definitions across calls.
func lintCriterion(name string, c rubricCriterion, owned map[string]bool, byDefinition map[string]string) []string {
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
	switch {
	case c.Deterministic && !owned[name]:
		problems = append(problems, "deterministic, but no check scores a criterion of this name")
	case !c.Deterministic && owned[name]:
		problems = append(problems, "a code check owns this name, so the judge's score is dropped; mark it deterministic: true")
	case !c.Deterministic && len(c.Bands) == 0:
		problems = append(problems, "no bands, so the judge has no pass bar to score against")
	}
	return problems
}
