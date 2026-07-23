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

// failsAtBase reports whether check ALSO fails against the repo's BASE tree -
// the pristine commit the worker cloned, before it touched anything.
//
// Why: a repo's derived checks can already fail on clean main (pre-existing
// lint debt), and under weakest-link scoring that makes the gate unwinnable. A
// node may only be failed for what its OWN change broke; a check that already
// fails at base is repo debt, so we don't gate on it.
//
// The check is re-run in a DETACHED GIT WORKTREE of the base commit, never in
// the worker's tree: no stash, no checkout, nothing that could lose the worker's
// uncommitted work (the one catastrophic failure mode here). The worker's
// node_modules is symlinked in so the re-run doesn't need a reinstall - and
// doesn't fail with a missing-dependency 127 that would look like a
// "pre-existing" failure.
//
// Conservative on error: if the base can't be determined or the check can't be
// run there, we report false - the check keeps gating, exactly as before this fix.
func failsAtBase(dir, check string, caps workspace.Caps) bool {
	base, err := baseCommit(dir, caps)
	if err != nil {
		slog.Debug("cannot determine base commit; check keeps gating", "component", "vetting", "dir", dir, "err", err)
		return false
	}
	key := dir + "\x00" + base + "\x00" + check
	if v, ok := baselineCache.Load(key); ok {
		return v.(bool)
	}
	fails, err := runAtBase(dir, base, check, caps)
	if err != nil {
		slog.Debug("cannot run check at base; check keeps gating", "component", "vetting", "check", check, "err", err)
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
	tmp, err := os.MkdirTemp("", "quack-base-")
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
