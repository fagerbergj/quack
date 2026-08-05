package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestPushBranchRecoversFromSurvivingRemoteBranch pins #714: a branch left
// over from a prior run on the same issue (the normal case on retrigger)
// must not fail delivery outright - PushBranch fetches it, rebases local
// work on top, and retries once.
func TestPushBranchRecoversFromSurvivingRemoteBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/issue-66") // prior run's surviving branch (adds pr.txt)
	runGitT(t, bare, "config", "receive.denyNonFastforwards", "true")

	b := newTestGitBinding(t)
	target := t.TempDir()
	runGitT(t, filepath.Dir(target), "clone", "--quiet", bare, target)
	runGitT(t, target, "checkout", "--quiet", "-b", "quack/issue-66") // fresh branch off main, unaware of the prior run
	runGitT(t, target, "config", "user.name", "test")
	runGitT(t, target, "config", "user.email", "test@x.local")
	if err := os.WriteFile(filepath.Join(target, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, target, "add", "-A")
	runGitT(t, target, "commit", "--quiet", "-m", "fix commit")

	if _, err := PushBranch(context.Background(), b.jail.Root(), target, "quack/issue-66", GitCredential{}, b.caps); err != nil {
		t.Fatalf("PushBranch: %v; want it to recover via fetch+rebase+retry", err)
	}

	fetched := t.TempDir()
	runGitT(t, filepath.Dir(fetched), "clone", "--quiet", bare, fetched)
	runGitT(t, fetched, "checkout", "--quiet", "quack/issue-66")
	if _, err := os.Stat(filepath.Join(fetched, "pr.txt")); err != nil {
		t.Error("recovered push lost the prior run's commit - want it preserved, not overwritten")
	}
	if _, err := os.Stat(filepath.Join(fetched, "fix.txt")); err != nil {
		t.Error("recovered push did not land this run's own commit")
	}
}

// TestPushBranchRebaseRecoveryFailureLeavesBranchAlone pins the other half of
// #714: when recovery itself can't resolve (a real conflict), PushBranch must
// give up cleanly - report the original push failure and leave the local
// branch and the remote exactly as they were, never a half-finished rebase
// or a partial push.
func TestPushBranchRebaseRecoveryFailureLeavesBranchAlone(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/issue-66") // prior run wrote pr.txt = "pr change"
	runGitT(t, bare, "config", "receive.denyNonFastforwards", "true")

	b := newTestGitBinding(t)
	target := t.TempDir()
	runGitT(t, filepath.Dir(target), "clone", "--quiet", bare, target)
	runGitT(t, target, "checkout", "--quiet", "-b", "quack/issue-66")
	runGitT(t, target, "config", "user.name", "test")
	runGitT(t, target, "config", "user.email", "test@x.local")
	// Same file, different content than the prior run - rebase can't replay this cleanly.
	if err := os.WriteFile(filepath.Join(target, "pr.txt"), []byte("conflicting change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, target, "add", "-A")
	runGitT(t, target, "commit", "--quiet", "-m", "conflicting commit")

	if _, err := PushBranch(context.Background(), b.jail.Root(), target, "quack/issue-66", GitCredential{}, b.caps); err == nil {
		t.Fatal("expected PushBranch to fail when rebase recovery hits a conflict")
	}

	if status := runGitT(t, target, "status", "--porcelain"); status != "" {
		t.Errorf("local tree left dirty after failed recovery: %q", status)
	}
	if _, statErr := os.Stat(filepath.Join(target, ".git", "rebase-merge")); statErr == nil {
		t.Error("a rebase was left in progress after recovery failed")
	}

	fetched := t.TempDir()
	runGitT(t, filepath.Dir(fetched), "clone", "--quiet", bare, fetched)
	runGitT(t, fetched, "checkout", "--quiet", "quack/issue-66")
	data, rerr := os.ReadFile(filepath.Join(fetched, "pr.txt"))
	if rerr != nil || string(data) != "pr change\n" {
		t.Errorf("remote branch was modified despite failed recovery: %q, err=%v", data, rerr)
	}
}
