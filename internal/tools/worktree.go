package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupWorktree provisions one node's git worktree, linked off the plan's
// shared setup clone. Idempotent: a resumed run finds its worktree already registered and moved to the clone's HEAD;
// untracked files survive. checkSetup bootstraps the worktree quack-side, before any sandboxed worker starts in it - a read-only worker (reviewer/explorer) can never run it itself, and the shared clone's own bootstrap (SetupClone) is not carried by `worktree add` for untracked state (e.g. an untracked vendor dir).
func SetupWorktree(ctx context.Context, jail *workspace.Jail, userID, chatID, parentDir, nodeRelDir, branch string, caps workspace.Caps, checkSetup []string) (string, error) {
	b := gitBinding{userID: userID, jail: jail, caps: caps}
	b.chatID = chatID
	target, err := b.resolve(nodeRelDir)
	if err != nil {
		return "", fmt.Errorf("setup: resolve worktree dir: %w", err)
	}
	if worktreeValid(target, parentDir) && syncWorktree(ctx, target, parentDir, caps) {
		workspace.PrecreateBuildDirs(target, caps.BuildDirs)
		workspace.RunCheckSetup(target, checkSetup, caps)
		return target, nil
	}
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("setup: clear stale worktree dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("setup: create worktree parent dir: %w", err)
	}
	// Best-effort: prune orphaned worktree metadata from a crashed run.
	_, _, _ = runGit(ctx, parentDir, []string{"worktree", "prune"}, caps, nil)
	if _, _, err := runGit(ctx, parentDir, []string{"worktree", "add", "--quiet", "-B", branch, target, "HEAD"}, caps, nil); err != nil {
		return "", fmt.Errorf("setup: worktree add %q: %w", branch, err)
	}
	// Before any sandboxed (possibly read-only) worker starts in target - see
	// PrecreateBuildDirs.
	workspace.PrecreateBuildDirs(target, caps.BuildDirs)
	workspace.RunCheckSetup(target, checkSetup, caps)
	return target, nil
}

// PruneWorktree detaches dir from the parent clone's bookkeeping before removal.
func PruneWorktree(ctx context.Context, dir string, caps workspace.Caps) error {
	common := workspace.WorktreeCommonGitDir(dir)
	if common == "" {
		return nil
	}
	parent := filepath.Dir(common)
	if _, err := os.Stat(parent); err != nil {
		return nil
	}
	_, _, err := runGit(ctx, parent, []string{"worktree", "remove", "--force", dir}, caps, nil)
	return err
}

// syncWorktree moves a reused worktree to the shared clone's current HEAD: only read-only
// nodes get one, so nothing of theirs lives in it, and a re-review after a push must read the new head.
func syncWorktree(ctx context.Context, target, parentDir string, caps workspace.Caps) bool {
	head, _, err := runGit(ctx, parentDir, []string{"rev-parse", "HEAD"}, caps, nil)
	if err != nil {
		return false
	}
	_, _, err = runGit(ctx, target, []string{"reset", "--quiet", "--hard", strings.TrimSpace(head)}, caps, nil)
	return err == nil
}

// worktreeValid: idempotency check for SetupWorktree.
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
