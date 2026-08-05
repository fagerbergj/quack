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

// baselineCache memoises "does this check ALSO fail at the base commit?" per
// (repo dir, base sha, check) - the answer can't change while the node runs
// (the base tree is immutable), so the node pays for a baseline at most once
// per check no matter how many revise rounds it burns.
var baselineCache sync.Map // key: dir\x00sha\x00check → bool

// failsAtBase reports whether check also fails on the repo's BASE tree, so
// pre-existing repo debt doesn't count against the worker under weakest-link
// scoring. Runs in a detached git worktree, never the worker's tree (avoids
// losing uncommitted work); node_modules is symlinked in so a missing
// dependency can't masquerade as debt. Conservative on error: still gates.
func failsAtBase(dir, check string, caps workspace.Caps) bool {
	base, err := baseCommit(dir, caps)
	if err != nil {
		// Warn, not Debug: the worker is now charged for a failure that may not be
		// its own, and it cannot see or fix why the probe didn't run.
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

// baseCommit is the commit the worker STARTED from: the clone's original HEAD,
// read from the oldest HEAD reflog entry (git clone writes exactly one, "clone:
// from <url>"). Deliberately not the current HEAD - by gate time the worker may
// have committed - and deliberately not a remote-tracking ref, which the worker's
// own push can move.
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

// runAtBase checks out base into a throwaway detached worktree and runs check
// there, returning whether it failed. The worktree is always removed.
func runAtBase(dir, base, check string, caps workspace.Caps) (bool, error) {
	// Under a sandbox the child git only reaches granted paths, and the server's
	// own /tmp is not one of them - hence SandboxTmpDir, not "".
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
	// Dependencies are gitignored, so the base worktree has none: reuse the
	// worker's installed tree rather than reinstalling (and rather than reading
	// a missing-dependency failure as pre-existing debt).
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
