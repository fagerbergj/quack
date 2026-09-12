package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupClone: harness-executed clone + branch checkout, run once before any
// node. checkSetup bootstraps the clone (e.g. `make plugins`) right after
// checkout, quack-side, before any sandboxed worker starts in it - the gate's checksPassCriterion reruns the same call, a no-op once this has (see workspace.RunCheckSetup's cache).
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
	// Before check_setup (which may itself populate node_modules/ - either
	// way MkdirAll on an existing populated dir is a no-op) and always before
	// any node's own ReadOnly caps can apply to this tree.
	workspace.PrecreateBuildDirs(target, caps.BuildDirs)
	workspace.RunCheckSetup(target, checkSetup, caps)
	return target, nil
}

// cleanupError: a stale-clone removal failure, never a fetch failure - the dag
// package structurally matches LocalCleanupFailure (dag never imports this
// package, see dag/graph.go) so its wrapping setupError doesn't reword it into a bogus "repository is unreachable" (#1213).
type cleanupError struct {
	path  string
	cause error
}

func (e *cleanupError) Error() string {
	return fmt.Sprintf("setup: could not clear stale clone dir %s: %v", e.path, e.cause)
}

func (e *cleanupError) Unwrap() error { return e.cause }

func (e *cleanupError) LocalCleanupFailure() {}

// localRefExists reports whether dir has a local branch named ref - used
// both to recognize a reusable clone (its baseRef branch survives the first
// checkout -b) and to tell a fresh workBranch from one a prior turn cut already.
func localRefExists(ctx context.Context, dir, ref string, caps workspace.Caps) bool {
	_, _, err := runGit(ctx, dir, []string{"show-ref", "--verify", "--quiet", "refs/heads/" + ref}, caps, nil)
	return err == nil
}

// canReuseClone reports whether target is already a healthy clone of repoURL
// cut from baseRef, so setupCloneAndBranch can skip the wipe+reclone. Any git
// failure here (missing dir, non-repo, wrong remote, corrupt tree left by a
// killed process) means "not reusable" - the caller falls back to a clean clone.
// It says nothing about whether baseRef is still current - runSetupCheckout
// fetches it fresh before ever cutting a new branch from it.
func canReuseClone(ctx context.Context, target, repoURL, baseRef string, caps workspace.Caps) bool {
	out, _, err := runGit(ctx, target, []string{"remote", "get-url", "origin"}, caps, nil)
	if err != nil || strings.TrimSpace(out) != repoURL {
		return false
	}
	return localRefExists(ctx, target, baseRef, caps)
}

// isShallowRepo reports whether dir is a shallow clone - `fetch --unshallow`
// errors on a repo that already isn't, which a reused review clone can be
// on its second setup call.
func isShallowRepo(ctx context.Context, dir string, caps workspace.Caps) bool {
	out, _, err := runGit(ctx, dir, []string{"rev-parse", "--is-shallow-repository"}, caps, nil)
	return err == nil && strings.TrimSpace(out) == "true"
}

// runSetupCheckout lands target on workBranch, given a clone that either was
// just cut fresh (reused=false, HEAD already sits on baseRef's tip) or is
// being reused from a prior turn (reused=true, so baseRef's local ref may be
// stale and the tree may carry state - dirty files, a mid-rebase, a detached
// HEAD - a plain checkout can refuse). It never forces past such a refusal;
// the caller treats any error here as "reuse failed" and re-clones instead.
func runSetupCheckout(ctx context.Context, b gitBinding, target, baseRef, workBranch string, checkoutExistingHead, reused bool) error {
	if checkoutExistingHead {
		if isShallowRepo(ctx, target, b.caps) {
			// Unshallow so three-dot diff has a merge-base.
			if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", "--unshallow", "origin"}, b.caps, nil); err != nil {
				return fmt.Errorf("setup: unshallow base history for review: %w", err)
			}
		}
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", "origin", workBranch + ":refs/remotes/origin/" + workBranch}, b.caps, nil); err != nil {
			return fmt.Errorf("setup: fetch review head %q: %w", workBranch, err)
		}
		if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-B", workBranch, "origin/" + workBranch}, b.caps, nil); err != nil {
			return fmt.Errorf("setup: checkout review head %q: %w", workBranch, err)
		}
		return nil
	}
	if reused && localRefExists(ctx, target, workBranch, b.caps) {
		// A prior turn already cut workBranch here - switch onto it as-is,
		// never -B, so its local (possibly unpushed) commits survive.
		if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", workBranch}, b.caps, nil); err != nil {
			return fmt.Errorf("setup: checkout %q: %w", workBranch, err)
		}
		return nil
	}
	if reused {
		// The clone is shallow and depth-1 from the first turn - baseRef's
		// local ref is frozen at that turn's tip. Fetch before cutting a new
		// branch from it, or every later turn silently misses upstream commits.
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", "origin", baseRef}, b.caps, nil); err != nil {
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

// finishSetup configures committer identity so a raw `git commit` works in target.
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
	target, err := b.resolve(dir)
	if err != nil {
		return "", fmt.Errorf("setup: resolve clone dir: %w", err)
	}
	// A follow-up turn on the same repo/base_ref finds its own prior clone
	// here - try to reuse it so a resumed node's local commits survive.
	if canReuseClone(ctx, target, repoURL, baseRef, b.caps) {
		if err := runSetupCheckout(ctx, b, target, baseRef, workBranch, checkoutExistingHead, true); err == nil {
			return finishSetup(ctx, b, target)
		}
		// Reuse couldn't land cleanly (dirty conflict, mid-rebase, detached
		// HEAD from a killed process) - fall through to the clean reclone
		// below rather than wedge every later turn on the same checkout error.
	}
	// Clear stale clone from a previous run. Local cleanup, not a fetch - its
	// error must never read as the repository being unreachable (#1213).
	if err := workspace.RemoveAllForce(target); err != nil {
		return "", &cleanupError{path: target, cause: err}
	}
	if _, err := b.cloneRepo(repoURL, dir, nil, baseRef); err != nil {
		return "", fmt.Errorf("setup: clone: %w", err)
	}
	if err := runSetupCheckout(ctx, b, target, baseRef, workBranch, checkoutExistingHead, false); err != nil {
		return "", err
	}
	return finishSetup(ctx, b, target)
}
