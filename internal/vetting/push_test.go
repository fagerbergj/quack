package vetting

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH")
	}
}

// runGitT is a test helper that fails the test on error.
func runGitT(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	out, _, err := runPushGit(context.Background(), dir, argv, workspace.DefaultCaps(), nil)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(argv, " "), err)
	}
	return out
}

// newBareRepoFixture creates a bare "remote" repo seeded with one commit on
// main containing README.md, entirely via runGitT - no network.
func newBareRepoFixture(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	runGitT(t, bare, "init", "--bare", "--initial-branch=main")

	seed := t.TempDir()
	runGitT(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, seed, "add", "-A")
	runGitT(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@x.local", "commit", "--quiet", "-m", "seed")
	runGitT(t, seed, "push", "--quiet", "origin", "main")
	return bare
}

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

// TestPushBranchRecoversFromSurvivingRemoteBranch pins #714: a branch left
// over from a prior run on the same issue must not fail delivery outright -
// PushBranch fetches it, rebases local work on top, and retries once.
func TestPushBranchRecoversFromSurvivingRemoteBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/issue-66") // prior run's surviving branch (adds pr.txt)
	runGitT(t, bare, "config", "receive.denyNonFastforwards", "true")

	jailRoot := t.TempDir()
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

	if _, err := PushBranch(context.Background(), jailRoot, target, "quack/issue-66", GitCredential{}, workspace.DefaultCaps()); err != nil {
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
// #714: when recovery itself can't resolve, PushBranch gives up cleanly.
func TestPushBranchRebaseRecoveryFailureLeavesBranchAlone(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/issue-66") // prior run wrote pr.txt = "pr change"
	runGitT(t, bare, "config", "receive.denyNonFastforwards", "true")

	jailRoot := t.TempDir()
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

	if _, err := PushBranch(context.Background(), jailRoot, target, "quack/issue-66", GitCredential{}, workspace.DefaultCaps()); err == nil {
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

// stubCredentialSource always returns a credential (or an error) for GitCredential.
type stubCredentialSource struct {
	cred *GitCredential
	err  error
}

func (s stubCredentialSource) GitCredential(context.Context, string) (*GitCredential, error) {
	return s.cred, s.err
}

// TestEnsurePushSkipsWhenNothingStagesAPush pins #452 at the new boundary:
// a review/comment-only delivery must never attempt a push, even with a
// real CloneDir and a working credential source.
func TestEnsurePushSkipsWhenNothingStagesAPush(t *testing.T) {
	dc := DeliveryContext{
		Branch:   "some-pr-branch",
		CloneDir: t.TempDir(), // not a git repo - a real push attempt would error
		Items:    []StagedDelivery{{Kind: "review"}},
	}
	cfg := Config{GitCredentials: stubCredentialSource{cred: &GitCredential{Token: "x"}}, Workspace: mustJail(t)}
	if err := ensurePush(context.Background(), cfg, &dc); err != nil {
		t.Fatalf("ensurePush: %v, want no-op for a review-only delivery", err)
	}
	if dc.PushedSHA != "" {
		t.Errorf("PushedSHA = %q, want empty - no push should have been attempted", dc.PushedSHA)
	}
}

// TestEnsurePushRequiresCredentialsWhenPushDemanded pins the delivery-boundary
// failure mode: a staged pull_request with no GitCredentials configured must
// fail loudly, never silently skip the push.
func TestEnsurePushRequiresCredentialsWhenPushDemanded(t *testing.T) {
	dc := DeliveryContext{
		Branch:   "feature",
		CloneDir: t.TempDir(),
		Items:    []StagedDelivery{{Kind: "pull_request", Title: "x"}},
	}
	cfg := Config{Workspace: mustJail(t)}
	if err := ensurePush(context.Background(), cfg, &dc); err == nil {
		t.Fatal("ensurePush: want an error - a push was demanded with no credential source configured")
	}
}

// TestEnsurePushSurfacesCredentialError pins that a credential-resolution
// failure is reported, not swallowed into a silent skip.
func TestEnsurePushSurfacesCredentialError(t *testing.T) {
	dc := DeliveryContext{
		Branch:   "feature",
		CloneURL: "https://github.com/acme/widgets.git",
		CloneDir: t.TempDir(),
		Items:    []StagedDelivery{{Kind: "pull_request", Title: "x"}},
	}
	cfg := Config{GitCredentials: stubCredentialSource{err: errors.New("no installation")}, Workspace: mustJail(t)}
	if err := ensurePush(context.Background(), cfg, &dc); err == nil {
		t.Fatal("ensurePush: want the credential source's error surfaced")
	}
}

func mustJail(t *testing.T) *workspace.Jail {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return j
}
