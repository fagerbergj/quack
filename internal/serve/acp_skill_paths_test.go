package serve

import (
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

// TestAcpSkillPathsNoDuplicateWhenOnDisk proves the by-name backfill rule: a
// plugin root that DOES resolve review-code on disk must not also get the
// extracted embedded copy appended - opencode's skill loader may error on a
// duplicate name.
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
