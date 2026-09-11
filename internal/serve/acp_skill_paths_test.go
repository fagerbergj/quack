package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAcpSkillPathsBackfillsEmbeddedDotagents proves #943: with no plugin
// roots configured (the distroless image, where dotagents isn't on disk),
// acpSkillPaths must still return a root under which review-code/SKILL.md
// exists - the same backfill newSkillSource already gives the in-process
// skill toolset.
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

// TestAcpSkillPathsNoDuplicateWhenOnDisk proves the by-name backfill rule: a plugin root
// that resolves review-code on disk must not also get the embedded copy appended -
// opencode's skill loader may error on a duplicate name.
func TestAcpSkillPathsNoDuplicateWhenOnDisk(t *testing.T) {
	vendor := t.TempDir()
	writePluginManifest(t, vendor, "review-code-standin")
	writeVendorSkill(t, filepath.Join(vendor, "skills"), "review-code", "standin for the vendored review-code skill")

	paths := acpSkillPaths(resolveSkillDirs([]string{vendor}))

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
	if len(scoped) != 1 || scoped[0].Name != "review-code" {
		t.Fatalf("acpSkillFrontmatters(..., [review-code]) = %v, want exactly the one named skill", scoped)
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
