package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAcpSkillPathsBackfillsEmbeddedDotagents: with no plugin roots (the distroless image), acpSkillPaths
// must still return a root holding review-code/SKILL.md, the same backfill newSkillSource gives.
func TestAcpSkillPathsBackfillsEmbeddedDotagents(t *testing.T) {
	paths := acpSkillPaths(nil)

	found := false
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(p, "review-code", "SKILL.md")); err == nil {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("acpSkillPaths(nil) = %v, want a root containing review-code/SKILL.md", paths)
	}
}

// TestAcpSkillPathsNoDuplicateWhenOnDisk: a plugin root under ANY name that has review-code must not also
// get the embedded copy - pi's skill loader sees bare directory names and may error on a duplicate.
func TestAcpSkillPathsNoDuplicateWhenOnDisk(t *testing.T) {
	vendor := t.TempDir()
	writePluginManifest(t, vendor, "review-code-standin")
	writeVendorSkill(t, filepath.Join(vendor, "skills"), "review-code", "standin for the vendored review-code skill")

	paths := acpSkillPaths(resolvePlugins([]string{vendor}))

	count := 0
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(p, "review-code", "SKILL.md")); err == nil {
			count++
		}
	}
	if count != 1 {
		t.Errorf("acpSkillPaths(%q) returned review-code %d times, want exactly 1 (the on-disk copy, no extracted duplicate): %v", vendor, count, paths)
	}
}

// TestAcpSkillFrontmatters_Scoped: a declared skills: list scopes the ACP
// roster to just those names.
func TestAcpSkillFrontmatters_Scoped(t *testing.T) {
	src := newSkillSource(nil)
	all, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}
	if len(all) < 2 {
		t.Fatalf("builtin skill source has only %d skills, need at least 2 to prove scoping", len(all))
	}

	scoped, err := acpSkillFrontmatters(context.Background(), src, []string{"review-code"})
	if err != nil {
		t.Fatalf("acpSkillFrontmatters: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Name != "quack:review-code" {
		t.Fatalf("acpSkillFrontmatters(..., [review-code]) = %v, want exactly quack:review-code (bare name, plugin-qualified result)", scoped)
	}
}

// TestAcpSkillFrontmatters_EmptyFallsBackToFullLibrary: no shipped ACP agent
// config declares skills: yet, so an empty list must still get every skill.
func TestAcpSkillFrontmatters_EmptyFallsBackToFullLibrary(t *testing.T) {
	src := newSkillSource(nil)
	all, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("ListFrontmatters: %v", err)
	}

	got, err := acpSkillFrontmatters(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("acpSkillFrontmatters: %v", err)
	}
	if len(got) != len(all) {
		t.Fatalf("acpSkillFrontmatters(..., nil) = %d skills, want the full library (%d)", len(got), len(all))
	}
}
