package vetting

import (
	"slices"
	"testing"
)

// promptlint fails a rubric whose deterministic criterion is missing here, so the list must match
// what codeOwnedCriterion accepts.
func TestCodeOwnedCriteriaMatchesCodeOwnedCriterion(t *testing.T) {
	names := CodeOwnedCriteria()
	if !slices.IsSorted(names) {
		t.Fatalf("CodeOwnedCriteria() not sorted: %v", names)
	}
	for _, want := range []string{"cites_sources", "specifics_cited", "specifics_supported", "checks_pass"} {
		if !slices.Contains(names, want) {
			t.Errorf("CodeOwnedCriteria() missing %q", want)
		}
	}
	for _, n := range names {
		if n == specificsCitedCriterion || n == specificsSupportedCriterion {
			continue // owned only once a rubric declares it
		}
		if !codeOwnedCriterion(n, nil) {
			t.Errorf("%q listed but codeOwnedCriterion rejects it", n)
		}
	}
}
