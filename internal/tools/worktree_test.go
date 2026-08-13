package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestSetupWorktreeCreatesDistinctDirsAndBranches pins the core of worktree-per-node isolation:
// two read-only qualifying nodes (reviewer, explorer) sharing one plan.Setup
// clone must each get their OWN directory AND their own branch - git refuses
// to check the same branch out in two worktrees at once, so a shared branch
// name would break the second node outright.
func TestSetupWorktreeCreatesDistinctDirsAndBranches(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir1, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, nil)
	if err != nil {
		t.Fatalf("SetupWorktree(review1): %v", err)
	}
	dir2, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("explore1"), workspace.WorktreeBranch("explore1"), b.caps, nil)
	if err != nil {
		t.Fatalf("SetupWorktree(explore1): %v", err)
	}

	if dir1 == dir2 {
		t.Fatalf("both nodes resolved to the SAME dir %q, want distinct worktrees", dir1)
	}
	if _, err := os.Stat(filepath.Join(dir1, "README.md")); err != nil {
		t.Errorf("worktree 1 missing the parent clone's content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "README.md")); err != nil {
		t.Errorf("worktree 2 missing the parent clone's content: %v", err)
	}

	branch1 := strings.TrimSpace(runGitT(t, dir1, "rev-parse", "--abbrev-ref", "HEAD"))
	branch2 := strings.TrimSpace(runGitT(t, dir2, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch1 == branch2 {
		t.Fatalf("both worktrees checked out the SAME branch %q, want distinct (git would refuse this for real)", branch1)
	}
	if branch1 != workspace.WorktreeBranch("review1") {
		t.Errorf("worktree 1 branch = %q, want %q", branch1, workspace.WorktreeBranch("review1"))
	}
	if branch2 != workspace.WorktreeBranch("explore1") {
		t.Errorf("worktree 2 branch = %q, want %q", branch2, workspace.WorktreeBranch("explore1"))
	}

	// Both worktrees are registered against the SAME parent clone.
	list := runGitT(t, parentDir, "worktree", "list", "--porcelain")
	if !strings.Contains(list, dir1) || !strings.Contains(list, dir2) {
		t.Errorf("git worktree list at the parent clone = %q, want both worktree dirs listed", list)
	}
}

// TestSetupWorktreeIsIdempotent pins the resumed-run requirement: re-entering
// the same node calls SetupWorktree again with the same arguments, and that
// must be a cheap no-op (the SAME worktree, still valid) rather than a
// disruptive re-link that could clobber files the worker already wrote.
func TestSetupWorktreeIsIdempotent(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	nodeRel := workspace.NodeDir("review1")
	branch := workspace.WorktreeBranch("review1")
	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	marker := filepath.Join(dir, "worker-wrote-this.txt")
	if err := os.WriteFile(marker, []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir2, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps, nil)
	if err != nil {
		t.Fatalf("second SetupWorktree (resume) must succeed, got: %v", err)
	}
	if dir2 != dir {
		t.Fatalf("second SetupWorktree resolved to a different dir: %q, want %q", dir2, dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("idempotent re-entry must NOT clobber the worker's own files: %v", err)
	}
}

// TestSetupWorktreeRunsCheckSetup pins the #856 follow-up: a read-only
// worktree (reviewer/explorer) can never bootstrap itself, so check_setup
// must run quack-side, in the worktree, before the worker's first round -
// not only later at gate-check time.
func TestSetupWorktreeRunsCheckSetup(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, []string{"touch generated.txt"})
	if err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err != nil {
		t.Errorf("check_setup did not run in the worktree before returning: %v", err)
	}
}

// TestSetupWorktreeRunsCheckSetupAfterSharedCloneAlreadyDid pins the per-dir
// cache key: workspace.RunCheckSetup's cache is shared across every caller
// (SetupClone and SetupWorktree both call into it), so a naive key (e.g. the
// parent clone's dir, or the node ID alone) would make the shared clone's
// bootstrap poison a worktree's own - exactly the live failure (a worktree
// missing scripts/node_modules despite the shared clone having "already run
// check_setup"). The key must be the worktree's OWN resolved dir.
func TestSetupWorktreeRunsCheckSetupAfterSharedCloneAlreadyDid(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	checkSetup := []string{"touch generated.txt"}

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}
	// Warm the cache for the SHARED clone's own dir first, mirroring
	// SetupClone's provisioning-time call.
	workspace.RunCheckSetup(parentDir, checkSetup, b.caps)
	if _, err := os.Stat(filepath.Join(parentDir, "generated.txt")); err != nil {
		t.Fatalf("check_setup did not run in the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, checkSetup)
	if err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err != nil {
		t.Errorf("check_setup did not run in the worktree - the shared clone's dir already being cached must not skip the worktree's own bootstrap: %v", err)
	}
}

// TestSetupWorktreeNoCheckSetupUnchanged pins that an unset check_setup
// leaves worktree provisioning byte-identical to before this call site existed.
func TestSetupWorktreeNoCheckSetupUnchanged(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, nil)
	if err != nil {
		t.Fatalf("SetupWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated.txt")); err == nil {
		t.Error("no check_setup configured, but a bootstrap artifact appeared anyway")
	}
}

// TestSetupWorktreeCheckSetupFailureWarnsAndProceeds pins the shared failure
// semantics with the gate's own check_setup call (checks.go): a broken
// bootstrap command must not fail node worktree provisioning.
func TestSetupWorktreeCheckSetupFailureWarnsAndProceeds(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	parentDir, err := setupCloneAndBranch(context.Background(), b, workspace.SetupCloneDir(workspace.SharedRepoScope),
		"file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup the shared clone: %v", err)
	}

	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps, []string{"false"})
	if err != nil {
		t.Fatalf("SetupWorktree must succeed despite a broken check_setup command, got: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("worktree missing after a failed check_setup: %v", err)
	}
}
