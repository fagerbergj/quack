package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupWorktree provisions one node's git worktree, linked off the plan's shared setup clone.
// Idempotent: a resumed run finds its worktree already registered. A gate-failed node's worktree is kept.
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
	// Best-effort: prune orphaned worktree metadata from a crashed run.
	_, _, _ = runGit(ctx, parentDir, []string{"worktree", "prune"}, caps, nil)
	if _, _, err := runGit(ctx, parentDir, []string{"worktree", "add", "--quiet", "-B", branch, target, "HEAD"}, caps, nil); err != nil {
		return "", fmt.Errorf("setup: worktree add %q: %w", branch, err)
	}
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
