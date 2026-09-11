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

// TestAcpSkillFrontmatters_Scoped proves perf audit finding 3: the ACP
// roster must be scoped to the agent's declared skills, same as the native
// branch's skillsource.Scoped call - not every builtin skill regardless of
// ac.Skills.
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

	none, err := acpSkillFrontmatters(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("acpSkillFrontmatters: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("acpSkillFrontmatters(..., nil) = %d skills, want 0 - an agent with no skills: key must not get the whole roster", len(none))
	}
}
