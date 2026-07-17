package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupCloneAndBranch is dag.Plan's declared Setup PRE-step, past URL
// validation — the deterministic twin of a worker's own git_clone +
// git_checkout -b, run once by the harness before any node.

func TestSetupCloneAndBranchClonesAndChecksOutNewBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work")
	if err != nil {
		t.Fatalf("setupCloneAndBranch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Errorf("clone did not land at %q: %v", target, err)
	}
	branchOut, _, err := runGit(context.Background(), target, []string{"rev-parse", "--abbrev-ref", "HEAD"}, b.caps, nil)
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(branchOut); got != "quack/work" {
		t.Errorf("checked-out branch = %q, want quack/work", got)
	}
	// Landed exactly where jail.Resolve(userID, chatID, dir) says — the same
	// place a worker's own git_clone(dir="n1/repo") would resolve to.
	want, err := b.jail.Resolve(b.userID, "", "n1/repo")
	if err != nil {
		t.Fatal(err)
	}
	if target != want {
		t.Errorf("clone dir = %q, want %q (the jail-resolved target)", target, want)
	}
}

func TestSetupCloneAndBranchFailsOnBadBaseRef(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	if _, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+bare, "no-such-branch", "quack/work"); err == nil {
		t.Fatal("expected an error for a base_ref that does not exist")
	}
}

func TestSetupCloneAndBranchFailsOnEmptyWorkBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	if _, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+bare, "main", ""); err == nil {
		t.Fatal("expected an error for an empty work_branch")
	}
}

func TestSetupCloneRejectsNonHTTPS(t *testing.T) {
	b := newTestGitBinding(t)
	if _, err := SetupClone(context.Background(), b.jail, b.userID, "", "repo", "file:///tmp/repo", "main", "quack/work", b.caps, nil, nil); err == nil {
		t.Error("expected SetupClone to reject a non-https repo URL")
	}
}
