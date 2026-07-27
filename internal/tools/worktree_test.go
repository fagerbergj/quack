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
		workspace.NodeDir("review1"), workspace.WorktreeBranch("review1"), b.caps)
	if err != nil {
		t.Fatalf("SetupWorktree(review1): %v", err)
	}
	dir2, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir,
		workspace.NodeDir("explore1"), workspace.WorktreeBranch("explore1"), b.caps)
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
	dir, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps)
	if err != nil {
		t.Fatalf("first SetupWorktree: %v", err)
	}
	marker := filepath.Join(dir, "worker-wrote-this.txt")
	if err := os.WriteFile(marker, []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir2, err := SetupWorktree(context.Background(), b.jail, b.userID, b.chatID, parentDir, nodeRel, branch, b.caps)
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
