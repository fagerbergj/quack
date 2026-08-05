package vetting

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/workspace"
)

// baselineCache: memoises "does this check fail at base?" per (dir, sha, check).
var baselineCache sync.Map // key: dir\x00sha\x00check → bool

// failsAtBase: does check fail on base tree? Pre-existing debt doesn't count against the worker. Conservative on error.
func failsAtBase(dir, check string, caps workspace.Caps) bool {
	base, err := baseCommit(dir, caps)
	if err != nil {
		// Worker may be charged for pre-existing failure.
		slog.Warn("cannot determine base commit; check keeps gating", "component", "vetting", "dir", dir, "err", err)
		return false
	}
	key := dir + "\x00" + base + "\x00" + check
	if v, ok := baselineCache.Load(key); ok {
		return v.(bool)
	}
	fails, err := runAtBase(dir, base, check, caps)
	if err != nil {
		slog.Warn("cannot run check at base; check keeps gating", "component", "vetting", "check", check, "err", err)
		return false
	}
	baselineCache.Store(key, fails)
	return fails
}

// baseCommit: the clone's original HEAD from the oldest reflog entry (not current HEAD or remote-tracking ref).
func baseCommit(dir string, caps workspace.Caps) (string, error) {
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "reflog", "show", "--format=%H", "HEAD"}, caps)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("git reflog: exit %d: %s", res.ExitCode, res.Output)
	}
	shas := strings.Fields(res.Output)
	if len(shas) == 0 {
		return "", fmt.Errorf("git reflog: no HEAD history in %s", dir)
	}
	return shas[len(shas)-1], nil
}

// runAtBase: runs check in a throwaway detached worktree at base.
func runAtBase(dir, base, check string, caps workspace.Caps) (bool, error) {
	// Use SandboxTmpDir under sandbox (server /tmp is not granted).
	tmp, err := os.MkdirTemp(workspace.SandboxTmpDir(caps), "quack-base-")
	if err != nil {
		return false, err
	}
	wt := filepath.Join(tmp, "wt")
	defer func() {
		_, _ = workspace.RunArgv(context.Background(), dir, []string{"git", "worktree", "remove", "--force", wt}, caps)
		_ = os.RemoveAll(tmp)
	}()
	res, err := workspace.RunArgv(context.Background(), dir, []string{"git", "worktree", "add", "--detach", wt, base}, caps)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("git worktree add %s: exit %d: %s", base, res.ExitCode, res.Output)
	}
	// Reuse worker's node_modules to avoid reinstall (gitignored deps absent at base).
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil {
		_ = os.Symlink(filepath.Join(dir, "node_modules"), filepath.Join(wt, "node_modules"))
	}
	stages, err := workspace.SplitPipeline(check)
	if err != nil {
		return false, err
	}
	out, err := workspace.RunPipeline(context.Background(), wt, stages, caps)
	if err != nil {
		return false, err
	}
	return out.ExitCode != 0, nil
}
