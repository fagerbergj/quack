package tools

import (
	"context"
	"fmt"
	"log/slog"
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
	if worktreeValid(target, parentDir) && syncWorktree(ctx, target, parentDir, branch, caps) {
		workspace.PrecreateBuildDirs(target, caps.BuildDirs)
		workspace.RunCheckSetup(target, checkSetup, caps)
		return target, nil
	}
	warnDiscard(parentDir, target)
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("setup: clear stale worktree dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("setup: create worktree parent dir: %w", err)
	}
	// Best-effort: prune orphaned worktree metadata from a crashed run.
	_, _, _ = runGit(ctx, parentDir, []string{"worktree", "prune"}, caps, nil)
	if _, _, err := runGitIn(ctx, parentDir, parentDir, []string{"worktree", "add", "--quiet", "-B", branch, target, "HEAD"}, caps, nil, target); err != nil {
		return "", fmt.Errorf("setup: worktree add %q: %w", branch, err)
	}
	// Before any sandboxed (possibly read-only) worker starts in target - see
	// PrecreateBuildDirs.
	workspace.PrecreateBuildDirs(target, caps.BuildDirs)
	workspace.RunCheckSetup(target, checkSetup, caps)
	return target, nil
}

// warnDiscard: recovery from a redirected clone or worktree is otherwise silent.
func warnDiscard(clone, dir string) {
	if err := workspace.RepoRedirect(clone, dir); err != nil {
		slog.Warn("git: discarding a tree that failed quack's repository checks; recreating it", "component", "tools", "dir", dir, "err", err)
	}
}

// PruneWorktree removes linked worktree dir and drops it from its owning clone's bookkeeping; that clone must lie
// inside root. quack removes dir itself so confined git needs no write grant on dir's parent.
func PruneWorktree(ctx context.Context, root, dir string, caps workspace.Caps) error {
	clone, err := workspace.WorktreeClone(root, dir)
	if clone == "" || err != nil {
		return err
	}
	if err := workspace.RemoveAllForce(dir); err != nil {
		return err
	}
	_, _, err = runGit(ctx, clone, []string{"worktree", "prune"}, caps, nil)
	return err
}

// syncWorktree moves a reused (read-only node's) worktree to the shared clone's HEAD on its own named branch;
// a plain reset would follow the agent-writable worktree HEAD, which can name a shared branch.
func syncWorktree(ctx context.Context, target, parentDir, branch string, caps workspace.Caps) bool {
	head, _, err := runGit(ctx, parentDir, []string{"rev-parse", "HEAD"}, caps, nil)
	if err != nil {
		return false
	}
	_, _, err = runGitIn(ctx, parentDir, target, []string{"checkout", "--quiet", "--force", "-B", branch, strings.TrimSpace(head)}, caps, nil)
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
