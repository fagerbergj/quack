package vetting

import "testing"

// TestBuildActivitySectionDeterministic proves buildActivitySection renders
// act.fetched (a map) in a stable order - a random map iteration order would
// relocate the cache-divergence point in the revise/continuation/finalize
// prompts on every call even when nothing about the activity changed.
func TestBuildActivitySectionDeterministic(t *testing.T) {
	act := workerActivity{fetched: map[string]struct{}{
		"https://api.github.com/repos/x/y/pulls/1304":     {},
		"https://raw.githubusercontent.com/x/y/main/a.go": {},
		"https://raw.githubusercontent.com/x/y/main/b.go": {},
		"https://raw.githubusercontent.com/x/y/main/c.go": {},
		"https://github.com/x/y/pull/1304/files":          {},
	}}
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		seen[buildActivitySection(act)]++
	}
	if len(seen) != 1 {
		t.Fatalf("distinct renderings of the same activity across 200 calls = %d, want 1", len(seen))
	}
}
