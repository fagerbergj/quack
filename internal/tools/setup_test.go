package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// setupCloneAndBranch is dag.Plan's declared Setup PRE-step, past URL
// validation - the deterministic twin of a worker's own git_clone +
// git_checkout -b, run once by the harness before any node.

func TestSetupCloneAndBranchClonesAndChecksOutNewBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work", false)
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
	// Landed exactly where jail.Resolve(userID, chatID, dir) says - the same
	// place a worker's own git_clone(dir="n1/repo") would resolve to.
	want, err := b.jail.Resolve(b.userID, "", "n1/repo")
	if err != nil {
		t.Fatal(err)
	}
	if target != want {
		t.Errorf("clone dir = %q, want %q (the jail-resolved target)", target, want)
	}
}

// TestSetupCloneAndBranchStaleCleanupFailureMessage is #1213: a stale clone
// dir left read-only by go's module cache must surface as a local-cleanup
// message, never worded as the repository being unreachable.
func TestSetupCloneAndBranchStaleCleanupFailureMessage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit; the failure this test reproduces cannot happen")
	}
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	target, err := b.jail.Resolve(b.userID, "", "repo")
	if err != nil {
		t.Fatal(err)
	}
	roDir := filepath.Join(target, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "f"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	if _, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+bare, "main", "quack/work", false); err != nil {
		t.Fatalf("setupCloneAndBranch should recover via RemoveAllForce: %v", err)
	}
}

// TestCleanupErrorMessageNeverClaimsUnreachable pins the exact wording #1213
// asks for and proves it never contains the fetch-failure "unreachable" text.
func TestCleanupErrorMessageNeverClaimsUnreachable(t *testing.T) {
	err := &cleanupError{path: "/workspace/local/x/quack-shared-repo", cause: os.ErrPermission}
	msg := err.Error()
	want := "setup: could not clear stale clone dir /workspace/local/x/quack-shared-repo: permission denied"
	if msg != want {
		t.Errorf("Error() = %q, want %q", msg, want)
	}
	if strings.Contains(msg, "unreachable") {
		t.Errorf("cleanup failure message must not claim the repository is unreachable: %q", msg)
	}
}

func TestSetupCloneAndBranchFailsOnBadBaseRef(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	if _, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+bare, "no-such-branch", "quack/work", false); err == nil {
		t.Fatal("expected an error for a base_ref that does not exist")
	}
}

func TestSetupCloneAndBranchFailsOnEmptyWorkBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	if _, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+bare, "main", "", false); err == nil {
		t.Fatal("expected an error for an empty work_branch")
	}
}

func TestSetupCloneRejectsNonHTTPS(t *testing.T) {
	b := newTestGitBinding(t)
	if _, err := SetupClone(context.Background(), b.jail, b.userID, "", "repo", "file:///tmp/repo", "main", "quack/work", false, b.caps, nil, nil, nil); err == nil {
		t.Error("expected SetupClone to reject a non-https repo URL")
	}
}

// TestSetupCloneRunsCheckSetup pins the #856 follow-up's other call site: the
// shared clone must be bootstrapped quack-side, right after checkout, so
// implementer nodes (which use it directly, no worktree) land in an
// already-bootstrapped tree without waiting for gate-check time. Exercises
// setupCloneAndBranch + workspace.RunCheckSetup directly (SetupClone's own
// composition) since SetupClone's https-only URL check has no local-fixture
// bypass, same as every other clone-behavior test in this file.
func TestSetupCloneRunsCheckSetup(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setupCloneAndBranch: %v", err)
	}
	workspace.RunCheckSetup(target, []string{"touch generated.txt"}, b.caps)
	if _, err := os.Stat(filepath.Join(target, "generated.txt")); err != nil {
		t.Errorf("check_setup did not run in the clone: %v", err)
	}
}

// A worker addressing a setup-provisioned clone with a PLAIN relative path
// (no "repo/" prefix, no absolute path) must resolve - the whole point of
// workspace.SetupCloneDir landing the clone AT the node's own root rather
// than a subdirectory of it. Before this, a worker had to either guess the
// "repo/" prefix or `cd` first; observed in production, it did neither and
// fell back to shelling out via run_command with an absolute path (`pwd`),
// escaping read_file/edit_file's windowing and loop guard.
func TestReadFileResolvesSetupCloneWithNoPrefix(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)

	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	gb := gitBinding{userID: "u1", chatID: "c1", jail: j, caps: workspace.DefaultCaps()}
	dir := workspace.SetupCloneDir("impl")
	if _, err := setupCloneAndBranch(context.Background(), gb, dir, "file://"+bare, "main", "quack/work", false); err != nil {
		t.Fatalf("setupCloneAndBranch: %v", err)
	}

	ctx := newGatedCtx(t, "plan-1", "impl", "c1")

	fb := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	res, err := fb.withCwd(ctx).readFile(readFileArgs{Path: "README.md"})
	if err != nil {
		t.Fatalf("read_file(\"README.md\") with no prefix: %v - want it to resolve directly into the setup clone", err)
	}
	if res.Content != "hello\n" {
		t.Errorf("Content = %q, want %q", res.Content, "hello\n")
	}

}

// TestReadFileResolvesSetupCloneLeadingSlash pins #502/#498: the trust-gate
// judge (same fs tools as the worker, see NewJudgeFactory) tried
// list_dir("/frontend") against a setup-provisioned clone and got "no such
// file" - jailPath's "/" branch still applies the node's own dir (nodeDir),
// but a call whose advisor-thread registration doesn't carry a WorkspaceNodeID
// must resolve identically either way. Registers exactly as dag/graph.go does
// for a repo-touching (implementer/reviewer) chain node - WorkspaceNodeID =
// workspace.SharedRepoScope, distinct from NodeID - the shape a judge's own
// invocation carries too (same token, same registration).
func TestReadFileResolvesSetupCloneLeadingSlash(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	fb := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}

	token := vetting.AdvisorThreadToken("plan-1", "reviewer-node")
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "reviewer-node", WorkspaceNodeID: workspace.SharedRepoScope, ChatID: "c1", SessionID: "c1",
	})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	ctx := &gatedCtx{fakeCtx: *newFakeCtx(), prompt: "review the PR\n\n" + vetting.AdvisorThreadMarker(token)}

	cloneDir, err := j.EnsureDir("u1", "c1", workspace.SetupCloneDir(workspace.SharedRepoScope))
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cloneDir, "frontend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "frontend", "App.tsx"), []byte("app"), 0o644); err != nil {
		t.Fatal(err)
	}

	rel, err := fb.withCwd(ctx).listDir(listDirArgs{Path: "frontend"})
	if err != nil {
		t.Fatalf("list_dir(\"frontend\"): %v", err)
	}
	abs, err := fb.withCwd(ctx).listDir(listDirArgs{Path: "/frontend"})
	if err != nil {
		t.Fatalf("list_dir(\"/frontend\"): %v - a leading slash must resolve inside the clone, not the chat root", err)
	}
	if len(abs.Entries) != len(rel.Entries) || len(abs.Entries) == 0 {
		t.Errorf("list_dir(\"/frontend\") entries = %v, want the same as the relative path %v", abs.Entries, rel.Entries)
	}
}

// TestSetupCloneAndBranchIsIdempotent pins the persistent-workspace bug: a
// re-labeled issue (or retried run) leaves a stale clone at the target, and a
// naive `git clone` fails exit 128 ("destination already exists"). Setup must
// clear the target and re-provision cleanly.
func TestSetupCloneAndBranchIsIdempotent(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	if _, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work", false); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	// Second provisioning at the SAME dir must succeed (clears the stale clone),
	// not fail because the directory already exists.
	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("second setup at an existing target must succeed, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Errorf("re-provisioned clone missing at %q: %v", target, err)
	}
}

// TestSetupCloneAndBranchConfiguresCommitterIdentity pins the git-identity gap:
// a worker that shells out to `git commit` (rather than the git_commit tool)
// needs a committer identity in the clone, or the commit fails exit 128
// ("Author identity unknown"). Setup must configure it.
func TestSetupCloneAndBranchConfiguresCommitterIdentity(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	// A plain `git commit` (no -c identity flags) must succeed - proving the
	// identity is configured in the clone.
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runGit(context.Background(), target, []string{"add", "-A"}, b.caps, nil); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, _, err := runGit(context.Background(), target, []string{"commit", "-m", "shelled-out commit"}, b.caps, nil); err != nil {
		t.Fatalf("a plain git commit must succeed after setup (identity not configured?): %v", err)
	}
}

// addBranchFixture pushes a new branch to bare, off its current main, with one
// extra commit - simulating a PR head that exists on the remote but was never
// created by this clone.
func addBranchFixture(t *testing.T, bare, branch string) {
	t.Helper()
	seed := t.TempDir()
	runGitT(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
	runGitT(t, seed, "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(seed, "pr.txt"), []byte("pr change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, seed, "add", "-A")
	runGitT(t, seed, "-c", "user.name=pr", "-c", "user.email=pr@x.local", "commit", "--quiet", "-m", "pr commit")
	runGitT(t, seed, "push", "--quiet", "origin", branch)
}

// TestSetupCloneAndBranchReviewChecksOutRealHeadCommits pins the bug behind
// #494: a review's Setup.WorkBranch names an EXISTING remote PR head, not a
// branch to create. `checkout -b` off base (the implement behavior) makes an
// empty LOCAL branch shadowing the real one, so the reviewer sees base with
// no diff. checkoutExistingHead=true must fetch the real branch and land on
// its actual commit, with base history present so a three-dot diff resolves.
func TestSetupCloneAndBranchReviewChecksOutRealHeadCommits(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "pr/head")
	b := newTestGitBinding(t)

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "pr/head", true)
	if err != nil {
		t.Fatalf("setupCloneAndBranch (review): %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "pr.txt")); err != nil {
		t.Errorf("checked-out HEAD is missing the PR head's own commit (pr.txt): %v - got a shadow branch off base instead", err)
	}
	branchOut, _, err := runGit(context.Background(), target, []string{"rev-parse", "--abbrev-ref", "HEAD"}, b.caps, nil)
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(branchOut); got != "pr/head" {
		t.Errorf("checked-out branch = %q, want pr/head", got)
	}
	// The PR diff must be computable - three-dot needs a merge-base, which
	// needs base's full history (the initial clone is shallow, base-only).
	diffOut, _, err := runGit(context.Background(), target, []string{"diff", "main...HEAD"}, b.caps, nil)
	if err != nil {
		t.Fatalf("git diff main...HEAD: %v, want a computable diff (merge-base present)", err)
	}
	if !strings.Contains(diffOut, "pr.txt") {
		t.Errorf("git diff main...HEAD = %q, want it to show the PR's pr.txt change", diffOut)
	}
}

// TestSetupCloneAndBranchImplementStillCreatesFreshBranch pins that the
// review fix left the implement path untouched: checkoutExistingHead=false
// still creates workBranch fresh off baseRef, even when a branch of that name
// already exists on the remote (a re-run/supersede must start clean, not
// continue the stale remote branch).
func TestSetupCloneAndBranchImplementStillCreatesFreshBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/work") // stale remote branch, same name
	b := newTestGitBinding(t)

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work", false)
	if err != nil {
		t.Fatalf("setupCloneAndBranch (implement): %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "pr.txt")); err == nil {
		t.Fatal("implement checkout picked up the stale remote branch's commit - want a fresh branch off base instead")
	}
	branchOut, _, err := runGit(context.Background(), target, []string{"rev-parse", "--abbrev-ref", "HEAD"}, b.caps, nil)
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if got := strings.TrimSpace(branchOut); got != "quack/work" {
		t.Errorf("checked-out branch = %q, want quack/work", got)
	}
}

// TestSetupThenPushPreservesExistingPRHeadCommit pins the invariant: a run
// bound to an existing PR branch must never rewrite that branch's history -
// new commits land ON TOP of what was already there. Without
// checkoutExistingHead, an implementer branches fresh off base, commits
// unrelated work, and PushBranch's required --force overwrites the remote
// branch outright, destroying the PR's real commit. With it, setup fetches
// and checks out that commit FIRST, so the push is a fast-forward.
func TestSetupThenPushPreservesExistingPRHeadCommit(t *testing.T) {
	requireGit(t)

	run := func(t *testing.T, checkoutExistingHead bool) (hasOriginal, hasNew bool) {
		t.Helper()
		bare := newBareRepoFixture(t)
		addBranchFixture(t, bare, "pr/head") // the PR's existing commit (pr.txt)
		b := newTestGitBinding(t)

		target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "pr/head", checkoutExistingHead)
		if err != nil {
			t.Fatalf("setupCloneAndBranch: %v", err)
		}
		// The worker's own new commit, on top of whatever setup checked out.
		if err := os.WriteFile(filepath.Join(target, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitT(t, target, "add", "-A")
		runGitT(t, target, "commit", "--quiet", "-m", "fix commit")

		if _, err := vetting.PushBranch(context.Background(), b.jail.Root(), target, "pr/head", vetting.GitCredential{}, b.caps); err != nil {
			t.Fatalf("PushBranch: %v", err)
		}

		fetched := t.TempDir()
		runGitT(t, filepath.Dir(fetched), "clone", "--quiet", bare, fetched)
		runGitT(t, fetched, "checkout", "--quiet", "pr/head")
		_, errOrig := os.Stat(filepath.Join(fetched, "pr.txt"))
		_, errNew := os.Stat(filepath.Join(fetched, "fix.txt"))
		return errOrig == nil, errNew == nil
	}

	t.Run("pre-#625 bug: checkoutExistingHead=false destroys the PR's existing commit", func(t *testing.T) {
		hasOriginal, hasNew := run(t, false)
		if hasOriginal {
			t.Error("original PR commit (pr.txt) survived - want it destroyed, this subtest documents the bug this invariant test guards against")
		}
		if !hasNew {
			t.Error("new commit (fix.txt) missing after push")
		}
	})

	t.Run("fixed: checkoutExistingHead=true preserves the PR's existing commit", func(t *testing.T) {
		hasOriginal, hasNew := run(t, true)
		if !hasOriginal {
			t.Fatal("original PR commit (pr.txt) was destroyed by setup+push - the invariant #625 exists to protect")
		}
		if !hasNew {
			t.Error("new commit (fix.txt) missing after push - work must land ON TOP of the existing head")
		}
	})
}
