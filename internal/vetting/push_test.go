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

// rawGit runs git directly (hermetic env) for scaffolding a fixture repo - init/clone into a
// not-yet-a-repo dir, which quack's runGit deliberately refuses.
func rawGit(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", argv, err, out)
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
	rawGit(t, bare, "init", "--bare", "--initial-branch=main")

	seed := t.TempDir()
	rawGit(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
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
	rawGit(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
	runGitT(t, seed, "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(seed, "pr.txt"), []byte("pr change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, seed, "add", "-A")
	runGitT(t, seed, "-c", "user.name=pr", "-c", "user.email=pr@x.local", "commit", "--quiet", "-m", "pr commit")
	runGitT(t, seed, "push", "--quiet", "origin", branch)
}

// TestPushBranchRecoversFromSurvivingRemoteBranch: a branch left from a prior run on the same issue
// doesn't fail delivery; PushBranch fetches it, rebases local work on top, and retries once.
func TestPushBranchRecoversFromSurvivingRemoteBranch(t *testing.T) {
	checkPushRecovers(t, func(string) {})
}

// TestConfinedPushBranchRecovers: the gate's push, fetch and rebase retry run with quack's git under Landlock, the
// local bare remote granted as a fixture.
func TestConfinedPushBranchRecovers(t *testing.T) {
	checkPushRecovers(t, func(bare string) {
		workspace.GitFixtureDirs = []string{bare}
		t.Cleanup(func() { workspace.GitFixtureDirs = nil; workspace.ConfineGit(false) })
		if !workspace.ConfineGit(true) {
			t.Skip("SKIPPING: landlock unavailable")
		}
	})
}

// checkPushRecovers runs the recovery; confine runs once the fixtures exist, before PushBranch.
func checkPushRecovers(t *testing.T, confine func(bare string)) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/issue-66") // prior run's surviving branch (adds pr.txt)
	rawGit(t, bare, "config", "receive.denyNonFastforwards", "true")

	jailRoot := t.TempDir()
	target := t.TempDir()
	rawGit(t, filepath.Dir(target), "clone", "--quiet", bare, target)
	runGitT(t, target, "checkout", "--quiet", "-b", "quack/issue-66") // fresh branch off main, unaware of the prior run
	runGitT(t, target, "config", "user.name", "test")
	runGitT(t, target, "config", "user.email", "test@x.local")
	if err := os.WriteFile(filepath.Join(target, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, target, "add", "-A")
	runGitT(t, target, "commit", "--quiet", "-m", "fix commit")

	confine(bare)
	if _, err := PushBranch(context.Background(), jailRoot, target, "file://"+bare, "quack/issue-66", GitCredential{}, workspace.DefaultCaps()); err != nil {
		t.Fatalf("PushBranch: %v; want it to recover via fetch+rebase+retry", err)
	}

	fetched := t.TempDir()
	rawGit(t, filepath.Dir(fetched), "clone", "--quiet", bare, fetched)
	runGitT(t, fetched, "checkout", "--quiet", "quack/issue-66")
	if _, err := os.Stat(filepath.Join(fetched, "pr.txt")); err != nil {
		t.Error("recovered push lost the prior run's commit - want it preserved, not overwritten")
	}
	if _, err := os.Stat(filepath.Join(fetched, "fix.txt")); err != nil {
		t.Error("recovered push did not land this run's own commit")
	}
}

// TestPushBranchRebaseRecoveryFailureLeavesBranchAlone: an unresolvable recovery gives up cleanly.
func TestPushBranchRebaseRecoveryFailureLeavesBranchAlone(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	addBranchFixture(t, bare, "quack/issue-66") // prior run wrote pr.txt = "pr change"
	rawGit(t, bare, "config", "receive.denyNonFastforwards", "true")

	jailRoot := t.TempDir()
	target := t.TempDir()
	rawGit(t, filepath.Dir(target), "clone", "--quiet", bare, target)
	runGitT(t, target, "checkout", "--quiet", "-b", "quack/issue-66")
	runGitT(t, target, "config", "user.name", "test")
	runGitT(t, target, "config", "user.email", "test@x.local")
	// Same file, different content than the prior run - rebase can't replay this cleanly.
	if err := os.WriteFile(filepath.Join(target, "pr.txt"), []byte("conflicting change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, target, "add", "-A")
	runGitT(t, target, "commit", "--quiet", "-m", "conflicting commit")

	if _, err := PushBranch(context.Background(), jailRoot, target, "file://"+bare, "quack/issue-66", GitCredential{}, workspace.DefaultCaps()); err == nil {
		t.Fatal("expected PushBranch to fail when rebase recovery hits a conflict")
	}

	if status := runGitT(t, target, "status", "--porcelain"); status != "" {
		t.Errorf("local tree left dirty after failed recovery: %q", status)
	}
	if _, statErr := os.Stat(filepath.Join(target, ".git", "rebase-merge")); statErr == nil {
		t.Error("a rebase was left in progress after recovery failed")
	}

	fetched := t.TempDir()
	rawGit(t, filepath.Dir(fetched), "clone", "--quiet", bare, fetched)
	runGitT(t, fetched, "checkout", "--quiet", "quack/issue-66")
	data, rerr := os.ReadFile(filepath.Join(fetched, "pr.txt"))
	if rerr != nil || string(data) != "pr change\n" {
		t.Errorf("remote branch was modified despite failed recovery: %q, err=%v", data, rerr)
	}
}

// TestPushBranchIgnoresRepoRedirectsAndHooks: a clone whose own config repoints origin, rewrites the
// validated URL, or sets core.hooksPath still pushes to that URL, and no hook runs.
func TestPushBranchIgnoresRepoRedirectsAndHooks(t *testing.T) {
	requireGit(t)
	bare, decoy := newBareRepoFixture(t), newBareRepoFixture(t)
	target := t.TempDir()
	rawGit(t, filepath.Dir(target), "clone", "--quiet", bare, target)
	runGitT(t, target, "checkout", "--quiet", "-b", "quack/issue-7")
	runGitT(t, target, "-c", "user.name=t", "-c", "user.email=t@x.local", "commit", "--quiet", "--allow-empty", "-m", "work")

	hooks, marker := t.TempDir(), filepath.Join(t.TempDir(), "hook-fired")
	for _, h := range []string{"pre-push", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGitT(t, target, "remote", "set-url", "origin", decoy)
	runGitT(t, target, "config", "url.file://"+decoy+".insteadOf", "file://"+bare)
	runGitT(t, target, "config", "remote.file://"+bare+".url", decoy)
	runGitT(t, target, "config", "core.hooksPath", hooks)

	if _, err := PushBranch(context.Background(), t.TempDir(), target, "file://"+bare, "quack/issue-7", GitCredential{}, workspace.DefaultCaps()); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	rawGit(t, bare, "rev-parse", "--verify", "--quiet", "refs/heads/quack/issue-7")
	if exec.Command("git", "-C", decoy, "rev-parse", "--verify", "--quiet", "refs/heads/quack/issue-7").Run() == nil {
		t.Error("branch landed in the decoy repo the clone's config pointed at")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("repo-level core.hooksPath hook ran during PushBranch (stat err=%v)", err)
	}
}

// TestPushBranchRejectsUnvalidatedURL: only a credential-free URL of the allowed transport is pushed to.
func TestPushBranchRejectsUnvalidatedURL(t *testing.T) {
	for _, u := range []string{"", "origin", "https://github.com/a/b.git", "file://tok@host/x"} {
		if _, err := PushBranch(context.Background(), t.TempDir(), t.TempDir(), u, "b", GitCredential{}, workspace.DefaultCaps()); err == nil {
			t.Errorf("PushBranch(%q): want a rejection", u)
		}
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

// TestEnsurePushSkipsWhenNothingStagesAPush: a review/comment-only delivery never pushes, even with
// a real CloneDir and working credentials.
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

// TestEnsurePushRequiresCredentialsWhenPushDemanded: a staged pull_request with no GitCredentials
// fails loudly, never silently skipping the push.
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
