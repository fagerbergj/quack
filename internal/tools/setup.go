package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupClone runs once before any node. checkSetup bootstraps the clone quack-side before any sandboxed
// worker starts; the gate's rerun is a no-op via workspace.RunCheckSetup's cache.
func SetupClone(ctx context.Context, jail *workspace.Jail, userID, chatID, dir, repoURL, baseRef, workBranch string, checkoutExistingHead bool, caps workspace.Caps, credentials []GitCredential, tokenSource GitTokenSource, checkSetup []string) (string, error) {
	if _, err := validateCloneURL(repoURL); err != nil {
		return "", err
	}
	b := gitBinding{userID: userID, jail: jail, caps: caps, credentials: credentials, tokenSource: tokenSource}
	b.chatID = chatID
	target, err := setupCloneAndBranch(ctx, b, dir, repoURL, baseRef, workBranch, checkoutExistingHead)
	if err != nil {
		return "", err
	}
	// Before check_setup and before any node's ReadOnly caps apply to this tree.
	workspace.PrecreateBuildDirs(target, caps.BuildDirs)
	workspace.RunCheckSetup(target, checkSetup, caps)
	return target, nil
}

// cleanupError structurally matches dag's LocalCleanupFailure, so dag never rewords a local
// cleanup failure as "repository is unreachable".
type cleanupError struct {
	path  string
	cause error
}

func (e *cleanupError) Error() string {
	return fmt.Sprintf("setup: could not clear stale clone dir %s: %v", e.path, e.cause)
}

func (e *cleanupError) Unwrap() error { return e.cause }

func (e *cleanupError) LocalCleanupFailure() {}

// unwipeableTreeError: the tree held work a wipe would destroy and was moved aside; never reword it
// as the repository being unreachable.
type unwipeableTreeError struct {
	target, movedTo, workBranch, reason string
	checkoutErr, moveErr                error
}

func (e *unwipeableTreeError) Error() string {
	if e.moveErr != nil {
		return fmt.Sprintf("setup: %s on %q could not be checked out cleanly (%v): %s - moving it aside also failed (%v) - resolve manually",
			e.target, e.workBranch, e.checkoutErr, e.reason, e.moveErr)
	}
	return fmt.Sprintf("setup: %s on %q could not be checked out cleanly (%v): %s - moved the tree to %s instead of discarding it",
		e.target, e.workBranch, e.checkoutErr, e.reason, e.movedTo)
}

func (e *unwipeableTreeError) Unwrap() error { return e.checkoutErr }

func (e *unwipeableTreeError) LocalCleanupFailure() {}

func localRefExists(ctx context.Context, dir, ref string, caps workspace.Caps) bool {
	_, _, err := runGit(ctx, dir, []string{"show-ref", "--verify", "--quiet", "refs/heads/" + ref}, caps, nil)
	return err == nil
}

// hasRebaseOrMergeState: a killed process can leave an interrupted rebase/merge under a clean `git status`.
func hasRebaseOrMergeState(dir string) bool {
	for _, p := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		if _, err := os.Stat(filepath.Join(dir, ".git", p)); err == nil {
			return true
		}
	}
	return false
}

// unpushedCommitCount fetches workBranch explicitly: a single-branch clone's refspec never covers it.
// ok=false (count unknown) must be treated as unpushed work.
func unpushedCommitCount(ctx context.Context, b gitBinding, target, repoURL, baseRef, workBranch string) (count int, ok bool) {
	if !localRefExists(ctx, target, workBranch, b.caps) {
		return 0, true
	}
	upstream := baseRef
	auth, err := b.authFor(repoURL)
	if err == nil {
		refspec := trackingRefspec(workBranch)
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", repoURL, refspec}, b.caps, auth); err == nil {
			upstream = "origin/" + workBranch
		}
	}
	out, _, err := runGit(ctx, target, []string{"rev-list", "--count", upstream + ".." + workBranch}, b.caps, nil)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, false
	}
	return n, true
}

// cleanStatusArgv: the flag outranks a .gitmodules ignore=none, so status never runs a child git in a nested repo
// (whose config is unstripped); a moved gitlink still shows.
var cleanStatusArgv = []string{"status", "--porcelain", "--untracked-files=no", "--ignore-submodules=dirty"}

// unwipeableReason: "" when target is safe to discard (untracked files alone don't count). A git failure
// counts as something to protect: an inability to check never excuses a wipe.
func unwipeableReason(ctx context.Context, b gitBinding, target, repoURL, baseRef, workBranch string) string {
	if hasRebaseOrMergeState(target) {
		return "a rebase or merge is in progress"
	}
	// An unreadable object store makes status report every file as changed; say what actually failed.
	if err := repoReadErr(ctx, target, b.caps); err != nil {
		slog.Warn("git: cannot read a clone's repository", "component", "tools", "dir", target, "err", err)
		return "git cannot read its repository: " + firstLine(err.Error())
	}
	out, _, err := runGit(ctx, target, cleanStatusArgv, b.caps, nil)
	if err != nil {
		return "its working tree state could not be checked"
	}
	if strings.TrimSpace(out) != "" {
		return "it has uncommitted changes"
	}
	n, ok := unpushedCommitCount(ctx, b, target, repoURL, baseRef, workBranch)
	if !ok {
		return "its commit history against origin could not be checked"
	}
	if n > 0 {
		return fmt.Sprintf("it has %d unpushed commit(s)", n)
	}
	return ""
}

// repoReadErr is why git cannot read target's refs and reachable objects; nil when it can, or target is absent or
// fails the repository checks (warnDiscard reports those).
func repoReadErr(ctx context.Context, target string, caps workspace.Caps) error {
	if _, err := os.Stat(target); err != nil || workspace.RepoRedirect(target, target) != nil {
		return nil
	}
	_, _, err := runGit(ctx, target, []string{"fsck", "--connectivity-only", "--no-dangling", "--no-progress"}, caps, nil)
	return err
}

// canReuseClone: any git failure means not reusable. It doesn't check baseRef is current;
// runSetupCheckout fetches it before cutting a branch.
func canReuseClone(ctx context.Context, target, repoURL, baseRef string, caps workspace.Caps) bool {
	out, _, err := runGit(ctx, target, []string{"remote", "get-url", "origin"}, caps, nil)
	if err != nil || strings.TrimSpace(out) != repoURL {
		return false
	}
	return localRefExists(ctx, target, baseRef, caps)
}

// isShallowRepo: `fetch --unshallow` errors on a repo that isn't, as a reused clone can be.
func isShallowRepo(ctx context.Context, dir string, caps workspace.Caps) bool {
	out, _, err := runGit(ctx, dir, []string{"rev-parse", "--is-shallow-repository"}, caps, nil)
	return err == nil && strings.TrimSpace(out) == "true"
}

// trackingRefspec: an explicit-URL fetch updates no remote-tracking ref on its own, unlike a fetch of "origin".
// The "+" lets a rebased, force-pushed branch replace the stale tracking ref a reused clone holds.
func trackingRefspec(ref string) string {
	return "+refs/heads/" + ref + ":refs/remotes/origin/" + ref
}

// runSetupCheckout never forces past a refused checkout (a reused tree may hold a dirty index);
// the caller treats any error as "reuse failed".
func runSetupCheckout(ctx context.Context, b gitBinding, target, repoURL, baseRef, workBranch string, checkoutExistingHead, reused bool) error {
	if checkoutExistingHead {
		auth, err := b.authFor(repoURL)
		if err != nil {
			return fmt.Errorf("setup: resolve credentials for %s: %w", repoURL, err)
		}
		if isShallowRepo(ctx, target, b.caps) {
			// Unshallow so three-dot diff has a merge-base.
			if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", "--unshallow", repoURL, trackingRefspec(baseRef)}, b.caps, auth); err != nil {
				return fmt.Errorf("setup: unshallow base history for review: %w", err)
			}
		}
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", repoURL, trackingRefspec(workBranch)}, b.caps, auth); err != nil {
			return fmt.Errorf("setup: fetch review head %q: %w", workBranch, err)
		}
		if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-B", workBranch, "origin/" + workBranch}, b.caps, nil); err != nil {
			return fmt.Errorf("setup: checkout review head %q: %w", workBranch, err)
		}
		return nil
	}
	if reused && localRefExists(ctx, target, workBranch, b.caps) {
		// Never -B: a prior turn's local, possibly unpushed commits must survive.
		if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", workBranch}, b.caps, nil); err != nil {
			return fmt.Errorf("setup: checkout %q: %w", workBranch, err)
		}
		return nil
	}
	if reused {
		// baseRef's local ref is frozen at the first turn's tip; fetch or later turns miss upstream commits.
		auth, err := b.authFor(repoURL)
		if err != nil {
			return fmt.Errorf("setup: resolve credentials for %s: %w", repoURL, err)
		}
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", repoURL, trackingRefspec(baseRef)}, b.caps, auth); err != nil {
			return fmt.Errorf("setup: fetch base ref %q: %w", baseRef, err)
		}
		if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-B", workBranch, "origin/" + baseRef}, b.caps, nil); err != nil {
			return fmt.Errorf("setup: checkout -b %q off %q: %w", workBranch, baseRef, err)
		}
		return nil
	}
	if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-b", workBranch}, b.caps, nil); err != nil {
		return fmt.Errorf("setup: checkout -b %q: %w", workBranch, err)
	}
	return nil
}

func finishSetup(ctx context.Context, b gitBinding, target string) (string, error) {
	for _, kv := range [][2]string{{"user.name", GitCommitAuthorName}, {"user.email", GitCommitAuthorEmail}} {
		if _, _, err := runGit(ctx, target, []string{"config", kv[0], kv[1]}, b.caps, nil); err != nil {
			return "", fmt.Errorf("setup: git config %s: %w", kv[0], err)
		}
	}
	return target, nil
}

// setupCloneAndBranch: false=create new branch off baseRef, true=checkout existing remote branch.
func setupCloneAndBranch(ctx context.Context, b gitBinding, dir, repoURL, baseRef, workBranch string, checkoutExistingHead bool) (string, error) {
	if strings.TrimSpace(baseRef) == "" {
		return "", fmt.Errorf("setup: base_ref must not be empty")
	}
	if err := validateRef(workBranch, "setup"); err != nil {
		return "", err
	}
	target, err := b.resolveRepoDir(dir)
	if err != nil {
		return "", fmt.Errorf("setup: resolve clone dir: %w", err)
	}
	// Reuse a prior turn's clone so a resumed node's local commits survive.
	if canReuseClone(ctx, target, repoURL, baseRef, b.caps) {
		checkoutErr := runSetupCheckout(ctx, b, target, repoURL, baseRef, workBranch, checkoutExistingHead, true)
		if checkoutErr == nil {
			return finishSetup(ctx, b, target)
		}
		// Reuse failed: a tree holding work at risk is moved aside, never wiped; anything else is recloned.
		if reason := unwipeableReason(ctx, b, target, repoURL, baseRef, workBranch); reason != "" {
			movedTo := target + ".unwiped-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
			moveErr := os.Rename(target, movedTo)
			return "", &unwipeableTreeError{target: target, movedTo: movedTo, workBranch: workBranch, reason: reason, checkoutErr: checkoutErr, moveErr: moveErr}
		}
	}
	// Local cleanup, not a fetch: its error must never read as the repository being unreachable.
	if err := repoReadErr(ctx, target, b.caps); err != nil {
		slog.Warn("git: re-cloning a clone git cannot read", "component", "tools", "dir", target, "err", err)
	}
	warnDiscard(target, target)
	if err := workspace.RemoveAllForce(target); err != nil {
		return "", &cleanupError{path: target, cause: err}
	}
	if err := b.cloneRepo(repoURL, dir, baseRef); err != nil {
		return "", fmt.Errorf("setup: clone: %w", err)
	}
	if err := runSetupCheckout(ctx, b, target, repoURL, baseRef, workBranch, checkoutExistingHead, false); err != nil {
		return "", err
	}
	return finishSetup(ctx, b, target)
}
