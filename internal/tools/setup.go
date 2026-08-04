package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupClone is the harness-executed twin of a worker's own git_clone plus a
// branch checkout - dag.Plan's declared Setup PRE-step, run once before any
// node. Authenticated via the SAME credential/tokenSource path
// git_clone/PushBranch use. repoURL must be a plain https:// URL
// (validateCloneURL) - a plan's Setup.Repo is orchestrator-declared, not
// operator-trusted, exactly like a worker's git_clone url. dir is
// workspace-relative (jail.Resolve-ready - see workspace.SetupCloneDir, which
// lands the clone AT the node's own root, unlike a worker's own git_clone,
// which typically lands one level under it). checkoutExistingHead selects
// which of the two branch checkouts setupCloneAndBranch runs (see its doc).
// Returns the resolved absolute clone dir.
func SetupClone(ctx context.Context, jail *workspace.Jail, userID, chatID, dir, repoURL, baseRef, workBranch string, checkoutExistingHead bool, caps workspace.Caps, credentials []GitCredential, tokenSource GitTokenSource) (string, error) {
	if _, err := validateCloneURL(repoURL); err != nil {
		return "", err
	}
	b := gitBinding{userID: userID, jail: jail, caps: caps, credentials: credentials, tokenSource: tokenSource}
	b.chatID = chatID
	return setupCloneAndBranch(ctx, b, dir, repoURL, baseRef, workBranch, checkoutExistingHead)
}

// setupCloneAndBranch is SetupClone past URL validation - split out so tests
// can drive it against a local (file://) remote, exactly like cloneRepo's own
// test seam (see cloneIntoJail in git_test.go).
//
// checkoutExistingHead distinguishes an IMPLEMENT setup from a REVIEW one:
//   - false (implement): workBranch is a NEW branch to create off baseRef -
//     `checkout -b`, unchanged from before this existed. A re-run/supersede
//     must start fresh from base, never continue a stale remote branch.
//   - true (review): workBranch is an EXISTING remote branch (a PR head) -
//     `checkout -b` off baseRef would create an empty LOCAL branch shadowing
//     the real remote one, leaving the reviewer looking at base with no diff
//     (the bug this guards against). Instead, fetch the real branch and check
//     it out, and unshallow so `git diff baseRef...HEAD` (three-dot, needs the
//     merge-base) is computable.
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
	// Idempotent provisioning: the workspace volume persists across runs, so a
	// re-labeled issue or a retried run can leave a stale clone here - git clone
	// would then fail exit 128 ("destination path already exists and is not an
	// empty directory"). Clear the target first so setup always starts clean.
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("setup: clear stale clone dir: %w", err)
	}
	if _, err := b.cloneRepo(repoURL, dir, nil, baseRef); err != nil {
		return "", fmt.Errorf("setup: clone: %w", err)
	}
	if checkoutExistingHead {
		// The initial clone is shallow (depth 1, base only) - unshallow so
		// baseRef's full history is present and a three-dot diff against it has
		// a merge-base to compute.
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", "--unshallow", "origin"}, b.caps, nil); err != nil {
			return "", fmt.Errorf("setup: unshallow base history for review: %w", err)
		}
		if _, _, err := runGit(ctx, target, []string{"fetch", "--quiet", "origin", workBranch + ":refs/remotes/origin/" + workBranch}, b.caps, nil); err != nil {
			return "", fmt.Errorf("setup: fetch review head %q: %w", workBranch, err)
		}
		if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-B", workBranch, "origin/" + workBranch}, b.caps, nil); err != nil {
			return "", fmt.Errorf("setup: checkout review head %q: %w", workBranch, err)
		}
	} else if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-b", workBranch}, b.caps, nil); err != nil {
		return "", fmt.Errorf("setup: checkout -b %q: %w", workBranch, err)
	}
	// A committer identity in the clone so a worker that shells out to
	// `git commit` (not just the git_commit tool, which passes its own via -c)
	// can commit - a fresh clone has none, and a raw commit without one fails
	// exit 128 ("Author identity unknown"). Same identity the git_commit tool uses.
	for _, kv := range [][2]string{{"user.name", GitCommitAuthorName}, {"user.email", GitCommitAuthorEmail}} {
		if _, _, err := runGit(ctx, target, []string{"config", kv[0], kv[1]}, b.caps, nil); err != nil {
			return "", fmt.Errorf("setup: git config %s: %w", kv[0], err)
		}
	}
	return target, nil
}
