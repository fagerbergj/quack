package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SetupClone: harness-executed clone + branch checkout, run once before any node.
func SetupClone(ctx context.Context, jail *workspace.Jail, userID, chatID, dir, repoURL, baseRef, workBranch string, checkoutExistingHead bool, caps workspace.Caps, credentials []GitCredential, tokenSource GitTokenSource) (string, error) {
	if _, err := validateCloneURL(repoURL); err != nil {
		return "", err
	}
	b := gitBinding{userID: userID, jail: jail, caps: caps, credentials: credentials, tokenSource: tokenSource}
	b.chatID = chatID
	return setupCloneAndBranch(ctx, b, dir, repoURL, baseRef, workBranch, checkoutExistingHead)
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
	// Clear stale clone from a previous run.
	if err := os.RemoveAll(target); err != nil {
		return "", fmt.Errorf("setup: clear stale clone dir: %w", err)
	}
	if _, err := b.cloneRepo(repoURL, dir, nil, baseRef); err != nil {
		return "", fmt.Errorf("setup: clone: %w", err)
	}
	if checkoutExistingHead {
		// Unshallow so three-dot diff has a merge-base.
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
	// Set committer identity so raw `git commit` works in the clone.
	for _, kv := range [][2]string{{"user.name", GitCommitAuthorName}, {"user.email", GitCommitAuthorEmail}} {
		if _, _, err := runGit(ctx, target, []string{"config", kv[0], kv[1]}, b.caps, nil); err != nil {
			return "", fmt.Errorf("setup: git config %s: %w", kv[0], err)
		}
	}
	return target, nil
}
