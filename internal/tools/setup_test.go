package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
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

// A worker addressing a setup-provisioned clone with a PLAIN relative path
// (no "repo/" prefix, no absolute path) must resolve — the whole point of
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
	if _, err := setupCloneAndBranch(context.Background(), gb, dir, "file://"+bare, "main", "quack/work"); err != nil {
		t.Fatalf("setupCloneAndBranch: %v", err)
	}

	ctx := newGatedCtx(t, "plan-1", "impl", "c1")

	fb := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	res, err := fb.withCwd(ctx).readFile(readFileArgs{Path: "README.md"})
	if err != nil {
		t.Fatalf("read_file(\"README.md\") with no prefix: %v — want it to resolve directly into the setup clone", err)
	}
	if res.Content != "hello\n" {
		t.Errorf("Content = %q, want %q", res.Content, "hello\n")
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

	if _, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work"); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	// Second provisioning at the SAME dir must succeed (clears the stale clone),
	// not fail because the directory already exists.
	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work")
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

	target, err := setupCloneAndBranch(context.Background(), b, "n1/repo", "file://"+bare, "main", "quack/work")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	// A plain `git commit` (no -c identity flags) must succeed — proving the
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
