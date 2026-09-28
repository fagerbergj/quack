package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// git_clone: https-only + credential-URL rejection (no network needed - both are rejected before any git process runs)

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
// gitEnv / GIT_ASKPASS injection shape
// ---------------------------------------------------------------------------

func TestGitEnvInjectsAskpassOnlyWithAuth(t *testing.T) {
	env := gitEnv(workspace.Caps{}, nil)
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_ASKPASS=") || strings.HasPrefix(e, GitAskpassTokenEnv+"=") || strings.HasPrefix(e, GitAskpassUserEnv+"=") {
			t.Errorf("no-auth env unexpectedly contains %q", e)
		}
	}

	auth := &gitAuth{
		cred:    GitCredential{Host: "github.com", Username: "x-access-token", Token: "secret"},
		askpass: "/workspace/" + GitAskpassLinkName,
		host:    "github.com",
	}
	env2 := gitEnv(workspace.Caps{}, auth)
	want := map[string]bool{
		GitAskpassHostEnv + "=github.com": false,
		// GIT_ASKPASS must be EXACTLY the executable symlink path - git execs the
		// value directly as one program, so any "<path> <arg>" form is a broken
		// (unexecutable) configuration: the regression guard for the live "cannot exec 'quack git-askpass'" failure.
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

// TestGitEnvIncludesWorkspaceEnv: workspace.env reaches git children too (a
// hook or filter may legitimately need the configured toolchain).
func TestGitEnvIncludesWorkspaceEnv(t *testing.T) {
	caps := workspace.Caps{Env: map[string]string{"JAVA_HOME": "/opt/jdk-21", "GIT_DIR": "/elsewhere", "GIT_CONFIG_COUNT": "1"}}
	env := gitEnv(caps, nil)
	if !slices.Contains(env, "JAVA_HOME=/opt/jdk-21") {
		t.Errorf("gitEnv = %v, want to contain JAVA_HOME", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_") {
			t.Errorf("gitEnv passed operator %q to quack's git", e)
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
	if b.credentialFor("https://GitHub.COM/a/b.git") == nil {
		t.Error("expected an ASCII case-insensitive match")
	}
	b.credentials = []GitCredential{{Host: "ks.example", Username: "x", Token: "t"}}
	if b.credentialFor("https://\u212a\u017f.example/a/b.git") != nil {
		t.Error("expected no Unicode case folding (Kelvin sign, long s)")
	}
}

// TestRunGitNeutralizesHooks: an executable .git/hooks/post-checkout that
// writes a marker file must NOT fire when runGit checks out a branch -
// otherwise it's an escape hatch a sandboxed ACP agent's own commit could trigger via this unconfined git binary.
func TestRunGitNeutralizesHooks(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	repo := t.TempDir()
	rawGit(t, filepath.Dir(repo), "clone", "--quiet", bare, repo)

	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	marker := filepath.Join(repo, "hook-fired")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	runGitT(t, repo, "checkout", "-b", "other-branch")

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("post-checkout hook fired - runGit did not neutralize .git/hooks")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// git_checkout - the reviewer's path to a PR branch. A shallow clone
// (--depth 1, which git implies --single-branch for) lands on the default branch
// ONLY: no other branch is reachable, so a code review of a PR was impossible before this tool existed.

// anyHostToken credentials every URL, so file:// fixtures take the credentialed path.
type anyHostToken struct{}

func (anyHostToken) GitCredential(context.Context, string) (*GitCredential, error) {
	return &GitCredential{Username: "u", Token: "tok"}, nil
}

// newDecoyRepoFixture is a bare repo whose main differs from newBareRepoFixture's by decoy.txt.
func newDecoyRepoFixture(t *testing.T) string {
	t.Helper()
	bare := newBareRepoFixture(t)
	seed := t.TempDir()
	rawGit(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "decoy.txt"), []byte("decoy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, seed, "add", "-A")
	runGitT(t, seed, "-c", "user.name=d", "-c", "user.email=d@x.local", "commit", "--quiet", "-m", "decoy")
	runGitT(t, seed, "push", "--quiet", "origin", "main")
	return bare
}

// TestCredentialedGitIgnoresHomeConfig: a url rewrite in the HOME git used to get (caps.HomeDir)
// must not steer the credentialed clone or the reuse fetch away from the requested repo.
func TestCredentialedGitIgnoresHomeConfig(t *testing.T) {
	requireGit(t)
	real, decoy := newBareRepoFixture(t), newDecoyRepoFixture(t)
	b := newTestGitBinding(t)
	b.tokenSource = anyHostToken{}
	b.caps.HomeDir = t.TempDir()
	rewrite := "[url \"file://" + decoy + "\"]\n\tinsteadOf = file://" + real + "\n"
	if err := os.WriteFile(filepath.Join(b.caps.HomeDir, ".gitconfig"), []byte(rewrite), 0o644); err != nil {
		t.Fatal(err)
	}
	wantMain := strings.TrimSpace(runGitT(t, real, "rev-parse", "main"))

	target, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+real, "main", "quack/one", false)
	if err != nil {
		t.Fatalf("setup (clone): %v", err)
	}
	// A second work branch reuses the clone and fetches main again.
	if _, err := setupCloneAndBranch(context.Background(), b, "repo", "file://"+real, "main", "quack/two", false); err != nil {
		t.Fatalf("setup (reuse fetch): %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "decoy.txt")); err == nil {
		t.Fatal("clone came from the rewritten decoy repo")
	}
	if got := strings.TrimSpace(runGitT(t, target, "rev-parse", "origin/main")); got != wantMain {
		t.Errorf("origin/main = %s, want the requested repo's main %s", got, wantMain)
	}
}

// TestRunGitIgnoresRepoHooksPath: a repo-level core.hooksPath must not fire during quack's own git calls.
func TestRunGitIgnoresRepoHooksPath(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	repo := t.TempDir()
	rawGit(t, filepath.Dir(repo), "clone", "--quiet", bare, repo)
	hooks, marker := t.TempDir(), filepath.Join(t.TempDir(), "hook-fired")
	for _, h := range []string{"post-checkout", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGitT(t, repo, "config", "core.hooksPath", hooks)

	runGitT(t, repo, "checkout", "-b", "other-branch")

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repo-level core.hooksPath hook ran (stat err=%v)", err)
	}
}

// TestRunGitStripsFilterDriverAndAttributes: a repo-level clean/smudge filter selected by .gitattributes
// runs arbitrary code on checkout/reset. quack's own checkout and reset must never fire it (config stripped).
func TestRunGitStripsFilterDriverAndAttributes(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	repo := t.TempDir()
	rawGit(t, filepath.Dir(repo), "clone", "--quiet", bare, repo)

	marker := filepath.Join(t.TempDir(), "filter-fired")
	touch := "#!/bin/sh\ntouch " + marker + "\ncat\n"
	drv := filepath.Join(t.TempDir(), "drv.sh")
	if err := os.WriteFile(drv, []byte(touch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repo, "config", "filter.evil.smudge", drv)
	runGitT(t, repo, "config", "filter.evil.clean", drv)

	runGitT(t, repo, "checkout", "-b", "other")
	runGitT(t, repo, "reset", "--hard", "HEAD")

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("filter driver ran during quack's checkout/reset (stat err=%v)", err)
	}
}

// TestRunGitNeutralizesGpgSign: a repo that forces commit.gpgSign with gpg.program pointing at a marker
// must produce an unsigned commit without running that program - the -c overrides win over repo config.
func TestRunGitNeutralizesGpgSign(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	repo := t.TempDir()
	rawGit(t, filepath.Dir(repo), "clone", "--quiet", bare, repo)

	marker := filepath.Join(t.TempDir(), "gpg-fired")
	prog := filepath.Join(t.TempDir(), "gpg.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repo, "config", "commit.gpgSign", "true")
	runGitT(t, repo, "config", "gpg.program", prog)
	runGitT(t, repo, "config", "user.name", "t")
	runGitT(t, repo, "config", "user.email", "t@x.local")

	runGitT(t, repo, "commit", "--quiet", "--allow-empty", "-m", "unsigned")

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("gpg.program ran during quack's commit (stat err=%v)", err)
	}
}

// TestGitEnvCarriesNoServerSecrets: the git child's env is built from gitEnv + GitCmd's pins only, never
// quack's os.Environ - so a QUACK_* server secret set in the parent never reaches the unsandboxed git process.
func TestGitEnvCarriesNoServerSecrets(t *testing.T) {
	requireGit(t)
	t.Setenv("QUACK_LLM_API_KEY", "super-secret")
	bin, err := gitBinaryPath()
	if err != nil {
		t.Skip("git not on PATH")
	}
	auth := &gitAuth{cred: GitCredential{Username: "u", Token: "tok"}, askpass: "/x/" + GitAskpassLinkName, host: "github.com"}
	cmd, done, err := workspace.GitCmd(context.Background(), bin, "", []string{"version"}, gitEnv(workspace.DefaultCaps(), auth))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "QUACK_") &&
			!strings.HasPrefix(e, GitAskpassTokenEnv+"=") &&
			!strings.HasPrefix(e, GitAskpassUserEnv+"=") &&
			!strings.HasPrefix(e, GitAskpassHostEnv+"=") {
			t.Errorf("git env carries a QUACK_* var beyond the askpass ones: %q", e)
		}
	}
}
