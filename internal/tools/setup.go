package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupClone is the harness-executed twin of a worker's own git_clone plus a
// new branch checkout — dag.Plan's declared Setup PRE-step, run once before
// any node. Authenticated via the SAME credential/tokenSource path
// git_clone/PushBranch use. repoURL must be a plain https:// URL
// (validateCloneURL) — a plan's Setup.Repo is orchestrator-declared, not
// operator-trusted, exactly like a worker's git_clone url. dir is
// workspace-relative (jail.Resolve-ready — see workspace.SetupCloneDir, which
// mirrors where that node's own git_clone would land). Returns the resolved
// absolute clone dir.
func SetupClone(ctx context.Context, jail *workspace.Jail, userID, chatID, dir, repoURL, baseRef, workBranch string, caps workspace.Caps, credentials []GitCredential, tokenSource GitTokenSource) (string, error) {
	if _, err := validateCloneURL(repoURL); err != nil {
		return "", err
	}
	b := gitBinding{userID: userID, jail: jail, caps: caps, credentials: credentials, tokenSource: tokenSource}
	b.chatID = chatID
	return setupCloneAndBranch(ctx, b, dir, repoURL, baseRef, workBranch)
}

// setupCloneAndBranch is SetupClone past URL validation — split out so tests
// can drive it against a local (file://) remote, exactly like cloneRepo's own
// test seam (see cloneIntoJail in git_test.go).
func setupCloneAndBranch(ctx context.Context, b gitBinding, dir, repoURL, baseRef, workBranch string) (string, error) {
	if strings.TrimSpace(baseRef) == "" {
		return "", fmt.Errorf("setup: base_ref must not be empty")
	}
	if err := validateRef(workBranch, "setup"); err != nil {
		return "", err
	}
	if _, err := b.cloneRepo(repoURL, dir, nil, baseRef); err != nil {
		return "", fmt.Errorf("setup: clone: %w", err)
	}
	target, err := b.resolve(dir)
	if err != nil {
		return "", fmt.Errorf("setup: resolve clone dir: %w", err)
	}
	if _, _, err := runGit(ctx, target, []string{"checkout", "--quiet", "-b", workBranch}, b.caps, nil); err != nil {
		return "", fmt.Errorf("setup: checkout -b %q: %w", workBranch, err)
	}
	return target, nil
}
