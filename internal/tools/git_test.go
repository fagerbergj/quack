package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// requireGit skips the test when the git binary isn't on PATH (dev sandboxes
// without git installed) rather than failing the whole suite.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH")
	}
}

func newTestGitBinding(t *testing.T) gitBinding {
	t.Helper()
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	return gitBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
}

// runGitT is a test helper that fails the test on error.
func runGitT(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	out, _, err := runGit(context.Background(), dir, argv, workspace.DefaultCaps(), nil)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(argv, " "), err)
	}
	return out
}

// newBareRepoFixture creates a bare "remote" repo (outside any jail) seeded
// with one commit on main containing README.md, entirely via runGit - no
// network. Returns the bare repo's path.
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

// cloneIntoJail clones bare into the jail at relDir via runGit directly
// (bypassing the git_clone TOOL's https-only enforcement - that's tested
// separately; this is fixture setup for a local, no-network round trip).
func cloneIntoJail(t *testing.T, b gitBinding, bare, relDir string) string {
	t.Helper()
	target, err := b.jail.Resolve(b.userID, "", relDir)
	if err != nil {
		t.Fatal(err)
	}
	userRoot, err := b.jail.Resolve(b.userID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, userRoot, "clone", "--quiet", bare, target)
	return target
}

// ---------------------------------------------------------------------------
// git_clone: https-only + credential-URL rejection (no network needed - both
// are rejected before any git process runs).
// ---------------------------------------------------------------------------

func TestGitCloneRejectsNonHTTPS(t *testing.T) {
	for _, url := range []string{
		"git@github.com:foo/bar.git",
		"ssh://git@github.com/foo/bar.git",
		"http://github.com/foo/bar.git",
		"file:///tmp/repo",
	} {
		if _, err := validateCloneURL(url); err == nil {
			t.Errorf("validateCloneURL(%q): expected https-only rejection, got nil error", url)
		}
	}
}

func TestGitCloneRejectsCredentialedURL(t *testing.T) {
	for _, url := range []string{
		"https://user:pass@github.com/foo/bar.git",
		"https://token@github.com/foo/bar.git",
	} {
		if _, err := validateCloneURL(url); err == nil {
			t.Errorf("validateCloneURL(%q): expected credential-in-url rejection, got nil error", url)
		} else if !strings.Contains(err.Error(), "credentials") {
			t.Errorf("validateCloneURL(%q): error = %v, want it to mention credentials", url, err)
		}
	}
}

func TestGitCloneAcceptsPlainHTTPS(t *testing.T) {
	// validateCloneURL only inspects the URL's scheme/userinfo - prove the
	// VALIDATION itself accepts a credential-free https URL (no network call;
	// the round-trip test below exercises the real clone path via runGit).
	u, err := validateCloneURL("https://github.com/example/repo.git")
	if err != nil {
		t.Fatalf("validateCloneURL: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https", u.Scheme)
	}
}

// ---------------------------------------------------------------------------
// Round trip: clone (via runGit fixture) -> status -> edit -> commit -> diff,
// all inside a temp jail, no network.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_commit's bulk-commit sanity wall (maxAddAllFiles): the deterministic
// guard against a blind `add_all` sweeping in garbage - the live incident
// this closes staged 1,261 npm-cache files alongside 8 real ones in one
// commit (see internal/workspace's HomeDir isolation fix for the OTHER half).
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Two users' jails: same relative path, fully independent contents.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_push guard matrix: disabled -> error; main -> rejected; no credential
// -> error.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_pull / git_rebase conflict -> auto-abort, repo state unchanged,
// conflicts listed.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_worktree_create / git_worktree_remove, including dirty-refuse.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// git_branch
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// gitEnv / GIT_ASKPASS injection shape
// ---------------------------------------------------------------------------

func TestGitEnvInjectsAskpassOnlyWithAuth(t *testing.T) {
	env := gitEnv("/home/x", workspace.Caps{}, nil)
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_ASKPASS=") || strings.HasPrefix(e, GitAskpassTokenEnv+"=") || strings.HasPrefix(e, GitAskpassUserEnv+"=") {
			t.Errorf("no-auth env unexpectedly contains %q", e)
		}
	}

	auth := &gitAuth{
		cred:    GitCredential{Host: "github.com", Username: "x-access-token", Token: "secret"},
		askpass: "/workspace/" + GitAskpassLinkName,
	}
	env2 := gitEnv("/home/x", workspace.Caps{}, auth)
	want := map[string]bool{
		// GIT_ASKPASS must be EXACTLY the executable symlink path - git execs
		// the value directly as one program, so any "<path> <arg>" form is a
		// broken (unexecutable) configuration. This is the regression guard
		// for the live "cannot exec 'quack git-askpass'" failure.
		"GIT_ASKPASS=/workspace/" + GitAskpassLinkName: false,
		GitAskpassUserEnv + "=x-access-token":          false,
		GitAskpassTokenEnv + "=secret":                 false,
	}
	for _, e := range env2 {
		if _, ok := want[e]; ok {
			want[e] = true
		}
		if strings.HasPrefix(e, "GIT_ASKPASS=") && strings.Contains(e, " ") {
			t.Errorf("GIT_ASKPASS value contains a space (unexecutable by git): %q", e)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("auth env missing %q (got %v)", k, env2)
		}
	}
}

// TestEnsureAskpassLink: the symlink is created pointing at the current
// executable, is stable across calls, and a stale link (pointing elsewhere)
// is repaired.
func TestEnsureAskpassLink(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	link, err := ensureAskpassLink(root)
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Join(root, GitAskpassLinkName) {
		t.Errorf("link path = %q, want it at the workspace root under %q", link, GitAskpassLinkName)
	}
	if dest, err := os.Readlink(link); err != nil || dest != self {
		t.Errorf("link -> %q (err=%v), want the current executable %q", dest, err, self)
	}

	// Idempotent second call.
	link2, err := ensureAskpassLink(root)
	if err != nil || link2 != link {
		t.Errorf("second call = (%q, %v), want the same link", link2, err)
	}

	// Stale link (binary moved) gets repaired.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/old-quack", link); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureAskpassLink(root); err != nil {
		t.Fatal(err)
	}
	if dest, err := os.Readlink(link); err != nil || dest != self {
		t.Errorf("stale link not repaired: -> %q (err=%v), want %q", dest, err, self)
	}
}

// TestAuthForCreatesAskpassLink: resolving a credentialed host yields an auth
// whose askpass path is a live symlink to this executable; a credential-less
// host yields nil auth and no error.
func TestAuthForCreatesAskpassLink(t *testing.T) {
	b := newTestGitBinding(t)
	b.credentials = []GitCredential{{Host: "github.com", Username: "u", Token: "tok"}}

	auth, err := b.authFor("https://github.com/a/b.git")
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		t.Fatal("expected auth for a credentialed host")
	}
	if auth.cred.Username != "u" || auth.cred.Token != "tok" {
		t.Errorf("auth.cred = %+v", auth.cred)
	}
	if fi, err := os.Lstat(auth.askpass); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("askpass %q is not a symlink (err=%v)", auth.askpass, err)
	}

	none, err := b.authFor("https://elsewhere.com/a/b.git")
	if err != nil || none != nil {
		t.Errorf("uncredentialed host: auth=%v err=%v, want nil/nil", none, err)
	}
}

func TestCredentialForMatchesExactHostOnly(t *testing.T) {
	b := newTestGitBinding(t)
	b.credentials = []GitCredential{{Host: "github.com", Username: "x", Token: "t"}}
	if b.credentialFor("https://github.com/a/b.git") == nil {
		t.Error("expected a match for github.com")
	}
	if b.credentialFor("https://notgithub.com/a/b.git") != nil {
		t.Error("expected no match for a different host")
	}
	if b.credentialFor("https://sub.github.com/a/b.git") != nil {
		t.Error("expected no match for a subdomain (exact host match only)")
	}
}

// ---------------------------------------------------------------------------
// git_checkout - the reviewer's path to a PR branch. A shallow clone
// (--depth 1, which git implies --single-branch for) lands on the default
// branch ONLY: no other branch is reachable, so a code review of a PR was
// impossible before this tool existed.
// ---------------------------------------------------------------------------
