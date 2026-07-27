package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupWorktree provisions ONE read-only qualifying node's (code-reviewer,
// code-explorer) own git worktree, linked off the plan's shared setup clone
// at parentDir (already resolved - see SetupClone) and checked out on branch
// (workspace.WorktreeBranch(nodeID) - unique per node, so git's
// same-branch-twice refusal never fires across siblings). nodeRelDir is
// workspace-relative (jail.Resolve-ready), exactly like SetupClone's dir.
//
// Idempotent: a resumed run re-entering the node finds its worktree already
// registered (worktreeValid) and returns it unchanged, never re-linking or
// resetting a worker's own in-progress files. A stale/partial leftover (a
// crashed prior attempt) is cleared and reprovisioned, mirroring
// setupCloneAndBranch's own idempotent-clean-then-create.
//
// A gate-failed node's worktree is KEPT, not reaped - matching a gate-failed
// node's plain directory today - so its contents stay inspectable; sweeping
// abandoned worktrees is a workspace GC task's job, not this one's.
func SetupWorktree(ctx context.Context, jail *workspace.Jail, userID, chatID, parentDir, nodeRelDir, branch string, caps workspace.Caps) (string, error) {
	b := gitBinding{userID: userID, jail: jail, caps: caps}
	b.chatID = chatID
	target, err := b.resolve(nodeRelDir)
	if err != nil {
		return "", fmt.Errorf("setup: resolve worktree dir: %w", err)
	}
	if worktreeValid(target, parentDir) {
		return target, nil
	}
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("setup: clear stale worktree dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("setup: create worktree parent dir: %w", err)
	}
	// Best-effort: drops metadata orphaned by a target directory removed out
	// from under git (a crashed run) rather than via `git worktree remove` -
	// left in place, that stale entry makes `add` at the same path fail with a
	// confusing "already registered" instead of the clean create below.
	_, _, _ = runGit(ctx, parentDir, []string{"worktree", "prune"}, caps, nil)
	if _, _, err := runGit(ctx, parentDir, []string{"worktree", "add", "--quiet", "-B", branch, target, "HEAD"}, caps, nil); err != nil {
		return "", fmt.Errorf("setup: worktree add %q: %w", branch, err)
	}
	return target, nil
}

// worktreeValid reports whether target is ALREADY a linked worktree of
// parentDir - SetupWorktree's idempotency check, so a resumed run's re-entry
// into an already-provisioned node is a cheap no-op rather than a disruptive
// re-link.
func worktreeValid(target, parentDir string) bool {
	common := workspace.WorktreeCommonGitDir(target)
	if common == "" {
		return false
	}
	parentGit, err := filepath.EvalSymlinks(filepath.Join(parentDir, ".git"))
	if err != nil {
		return false
	}
	return common == filepath.Clean(parentGit)
}
