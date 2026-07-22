package vetting

import (
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestResolveCiteCloneRootsSetupProvisioned mirrors
// TestCommitDeliveryOnPassUsesSetupBranchWhenDeclared's wiring: a
// setup-provisioned node (cfg.Setup set) never git_clone's anything itself, so
// act.clonedDirs is empty - resolveCiteCloneRoots must still resolve the
// harness-provisioned SetupCloneDir so citationScore has a real root to
// disk-verify against (the #437 fix's actual wiring point).
func TestResolveCiteCloneRootsSetupProvisioned(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cloneDir, err := j.EnsureDir("u1", "chat1", workspace.SetupCloneDir("impl"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Setup:           &SetupBranch{Repo: "https://github.com/fagerbergj/games", WorkBranch: "quack/work"},
		NodeID:          "impl",
		Workspace:       j,
		WorkspaceUserID: "u1",
		ChatID:          "chat1",
	}
	roots := resolveCiteCloneRoots(cfg, workerActivity{}) // empty ledger, on purpose
	if len(roots) != 1 || roots[0] != cloneDir {
		t.Fatalf("roots = %v, want exactly [%s]", roots, cloneDir)
	}
}

// TestResolveCiteCloneRootsIncludesWorkerClones: a node that git_clone'd its
// own repo (no cfg.Setup) still gets that clone resolved as a root.
func TestResolveCiteCloneRootsIncludesWorkerClones(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cloneDir, err := j.EnsureDir("u1", "chat1", "games-repo")
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{Workspace: j, WorkspaceUserID: "u1", ChatID: "chat1"}
	roots := resolveCiteCloneRoots(cfg, workerActivity{clonedDirs: []string{"games-repo"}})
	if len(roots) != 1 || roots[0] != cloneDir {
		t.Fatalf("roots = %v, want exactly [%s]", roots, cloneDir)
	}
}

// TestResolveCiteCloneRootsNoWorkspaceIsEmpty: no workspace wired up (a
// research/synthesis node) resolves to no clone roots - citationScore then
// falls back to pure web-URL ledger scoring, unchanged.
func TestResolveCiteCloneRootsNoWorkspaceIsEmpty(t *testing.T) {
	if roots := resolveCiteCloneRoots(Config{}, workerActivity{}); roots != nil {
		t.Errorf("roots = %v, want nil with no workspace wired up", roots)
	}
}
