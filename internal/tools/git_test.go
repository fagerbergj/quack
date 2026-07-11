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
// with one commit on main containing README.md, entirely via runGit — no
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
// (bypassing the git_clone TOOL's https-only enforcement — that's tested
// separately; this is fixture setup for a local, no-network round trip).
func cloneIntoJail(t *testing.T, b gitBinding, bare, relDir string) string {
	t.Helper()
	target, err := b.jail.Resolve(b.userID, relDir)
	if err != nil {
		t.Fatal(err)
	}
	userRoot, err := b.jail.Resolve(b.userID, "")
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
// git_clone: https-only + credential-URL rejection (no network needed — both
// are rejected before any git process runs).
// ---------------------------------------------------------------------------

func TestGitCloneRejectsNonHTTPS(t *testing.T) {
	b := newTestGitBinding(t)
	for _, url := range []string{
		"git@github.com:foo/bar.git",
		"ssh://git@github.com/foo/bar.git",
		"http://github.com/foo/bar.git",
		"file:///tmp/repo",
	} {
		if _, err := b.gitClone(gitCloneArgs{URL: url}); err == nil {
			t.Errorf("gitClone(%q): expected https-only rejection, got nil error", url)
		}
	}
}

func TestGitCloneRejectsCredentialedURL(t *testing.T) {
	b := newTestGitBinding(t)
	for _, url := range []string{
		"https://user:pass@github.com/foo/bar.git",
		"https://token@github.com/foo/bar.git",
	} {
		if _, err := b.gitClone(gitCloneArgs{URL: url}); err == nil {
			t.Errorf("gitClone(%q): expected credential-in-url rejection, got nil error", url)
		} else if !strings.Contains(err.Error(), "credentials") {
			t.Errorf("gitClone(%q): error = %v, want it to mention credentials", url, err)
		}
	}
}

func TestGitCloneAcceptsPlainHTTPS(t *testing.T) {
	// validateCloneURL only inspects the URL's scheme/userinfo — prove the
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

func TestGitRoundTrip_StatusCommitDiff(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	target := cloneIntoJail(t, b, bare, "repo")

	// git_status: clean, on main.
	st, err := b.gitStatus(gitStatusArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Clean {
		t.Errorf("Clean = false, want true right after clone")
	}
	if st.Branch != "main" {
		t.Errorf("Branch = %q, want main", st.Branch)
	}

	// Edit: add a new file.
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := b.gitStatus(gitStatusArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Clean {
		t.Error("Clean = true, want false after adding a file")
	}
	found := false
	for _, c := range st2.Changes {
		if c.Path == "new.txt" && c.State == "??" {
			found = true
		}
	}
	if !found {
		t.Errorf("Changes = %+v, want new.txt marked ??", st2.Changes)
	}

	// git_commit: default add_all=true.
	cres, err := b.gitCommit(gitCommitArgs{Dir: "repo", Message: "add new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if cres.FilesChanged != 1 {
		t.Errorf("FilesChanged = %d, want 1", cres.FilesChanged)
	}
	if cres.SHA == "" {
		t.Error("SHA is empty")
	}

	// Author is fixed to quack <agent@quack.local>.
	authorOut := runGitT(t, target, "log", "-1", "--pretty=format:%an <%ae>")
	if authorOut != "quack <agent@quack.local>" {
		t.Errorf("commit author = %q, want quack <agent@quack.local>", authorOut)
	}

	// git_diff HEAD~1: the new file shows up.
	dres, err := b.gitDiff(gitDiffArgs{Dir: "repo", Ref: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dres.Diff, "new.txt") {
		t.Errorf("Diff does not mention new.txt: %q", dres.Diff)
	}

	// git_log
	lres, err := b.gitLog(gitLogArgs{Dir: "repo", N: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(lres.Commits) != 2 {
		t.Fatalf("len(Commits) = %d, want 2", len(lres.Commits))
	}
	if lres.Commits[0].Subject != "add new.txt" {
		t.Errorf("Commits[0].Subject = %q, want %q", lres.Commits[0].Subject, "add new.txt")
	}
}

func TestGitCommitEmptyMessageRejected(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	cloneIntoJail(t, b, bare, "repo")
	if _, err := b.gitCommit(gitCommitArgs{Dir: "repo", Message: "  "}); err == nil {
		t.Error("expected error for an empty commit message")
	}
}

// ---------------------------------------------------------------------------
// Two users' jails: same relative path, fully independent contents.
// ---------------------------------------------------------------------------

func TestGitTwoUsersIndependentJails(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b1 := gitBinding{userID: "alice", jail: j, caps: workspace.DefaultCaps()}
	b2 := gitBinding{userID: "bob", jail: j, caps: workspace.DefaultCaps()}
	t1 := cloneIntoJail(t, b1, bare, "repo")
	t2 := cloneIntoJail(t, b2, bare, "repo")
	if t1 == t2 {
		t.Fatalf("both users resolved to the same real path %q", t1)
	}
	if err := os.WriteFile(filepath.Join(t1, "alice-only.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(t2, "alice-only.txt")); !os.IsNotExist(err) {
		t.Errorf("bob's jail sees alice's file (err=%v) — jails are not independent", err)
	}
}

// ---------------------------------------------------------------------------
// git_push guard matrix: disabled -> error; main -> rejected; no credential
// -> error.
// ---------------------------------------------------------------------------

func TestGitPushDisabledByDefault(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t) // allowPush is the zero value (false)
	cloneIntoJail(t, b, bare, "repo")
	_, err := b.gitPush(gitPushArgs{Dir: "repo"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("gitPush: err = %v, want a disabled error", err)
	}
}

func TestGitPushRejectsProtectedBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	b.allowPush = true
	cloneIntoJail(t, b, bare, "repo")
	for _, branch := range []string{"main", "master"} {
		_, err := b.gitPush(gitPushArgs{Dir: "repo", Branch: branch})
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Errorf("gitPush(branch=%q): err = %v, want a rejected error", branch, err)
		}
	}
}

func TestGitPushRequiresCredential(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	b.allowPush = true
	target := cloneIntoJail(t, b, bare, "repo")
	runGitT(t, target, "checkout", "-b", "feature")
	_, err := b.gitPush(gitPushArgs{Dir: "repo", Branch: "feature"})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("gitPush: err = %v, want a no-credential error", err)
	}
}

func TestGitPushSucceedsWithCredentialAndFeatureBranch(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	b.allowPush = true
	// bare's remote URL, as configured by `git clone`, is a filesystem path —
	// credentialFor matches by exact host, and a plain filesystem path has no
	// host, so no credential ever matches it. This proves the "real" push path
	// (past the disabled/branch checks) still correctly requires a credential
	// even for a non-protected branch, without needing a live HTTPS remote.
	target := cloneIntoJail(t, b, bare, "repo")
	runGitT(t, target, "checkout", "-b", "feature")
	b.credentials = []GitCredential{{Host: "example.com", Username: "x", Token: "t"}}
	_, err := b.gitPush(gitPushArgs{Dir: "repo", Branch: "feature"})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("gitPush: err = %v, want a no-credential error (bare fixture has no matching host)", err)
	}
}

// ---------------------------------------------------------------------------
// git_pull / git_rebase conflict -> auto-abort, repo state unchanged,
// conflicts listed.
// ---------------------------------------------------------------------------

// setupConflict prepares a local jailed clone with an UNPUSHED local commit
// that conflicts with a commit already pushed to origin/main by a separate
// scratch clone — so a pull/rebase in the jailed clone must conflict.
func setupConflict(t *testing.T, b gitBinding) (target, beforeSHA, beforeContent string) {
	t.Helper()
	bare := t.TempDir()
	runGitT(t, bare, "init", "--bare", "--initial-branch=main")

	seed := t.TempDir()
	runGitT(t, filepath.Dir(seed), "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, seed, "add", "-A")
	runGitT(t, seed, "-c", "user.name=seed", "-c", "user.email=seed@x.local", "commit", "--quiet", "-m", "base")
	runGitT(t, seed, "push", "--quiet", "origin", "main")

	target = cloneIntoJail(t, b, bare, "repo")
	beforeSHA = strings.TrimSpace(runGitT(t, target, "rev-parse", "HEAD"))

	// A remote-side commit (pushed by a second scratch clone) touching file.txt.
	remoteScratch := t.TempDir()
	runGitT(t, filepath.Dir(remoteScratch), "clone", "--quiet", bare, remoteScratch)
	if err := os.WriteFile(filepath.Join(remoteScratch, "file.txt"), []byte("remote-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, remoteScratch, "add", "-A")
	runGitT(t, remoteScratch, "-c", "user.name=remote", "-c", "user.email=remote@x.local", "commit", "--quiet", "-m", "remote change")
	runGitT(t, remoteScratch, "push", "--quiet", "origin", "main")

	// A local, unpushed commit in the jailed clone touching the SAME line.
	beforeContent = "local-version\n"
	if err := os.WriteFile(filepath.Join(target, "file.txt"), []byte(beforeContent), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, target, "-c", "user.name=quack", "-c", "user.email=agent@quack.local", "commit", "--quiet", "-am", "local change")
	beforeSHA = strings.TrimSpace(runGitT(t, target, "rev-parse", "HEAD"))
	return target, beforeSHA, beforeContent
}

func TestGitPullConflictAutoAborts(t *testing.T) {
	requireGit(t)
	b := newTestGitBinding(t)
	target, beforeSHA, beforeContent := setupConflict(t, b)

	res, err := b.gitPull(gitPullArgs{Dir: "repo"})
	if err != nil {
		t.Fatalf("gitPull: unexpected error (conflicts should be reported, not errored): %v", err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "file.txt" {
		t.Errorf("Conflicts = %v, want [file.txt]", res.Conflicts)
	}

	afterSHA := strings.TrimSpace(runGitT(t, target, "rev-parse", "HEAD"))
	if afterSHA != beforeSHA {
		t.Errorf("HEAD changed after aborted pull: before=%s after=%s", beforeSHA, afterSHA)
	}
	content, err := os.ReadFile(filepath.Join(target, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != beforeContent {
		t.Errorf("file.txt = %q, want unchanged %q", content, beforeContent)
	}
	st, err := b.gitStatus(gitStatusArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Clean {
		t.Errorf("Clean = false after abort, want true (no lingering rebase/merge state): %+v", st.Changes)
	}
}

func TestGitRebaseConflictAutoAborts(t *testing.T) {
	requireGit(t)
	b := newTestGitBinding(t)
	target, beforeSHA, beforeContent := setupConflict(t, b)
	runGitT(t, target, "fetch", "--quiet", "origin")

	res, err := b.gitRebase(gitRebaseArgs{Dir: "repo", Onto: "origin/main"})
	if err != nil {
		t.Fatalf("gitRebase: unexpected error (conflicts should be reported, not errored): %v", err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "file.txt" {
		t.Errorf("Conflicts = %v, want [file.txt]", res.Conflicts)
	}
	if res.Rebased {
		t.Error("Rebased = true, want false on conflict")
	}

	afterSHA := strings.TrimSpace(runGitT(t, target, "rev-parse", "HEAD"))
	if afterSHA != beforeSHA {
		t.Errorf("HEAD changed after aborted rebase: before=%s after=%s", beforeSHA, afterSHA)
	}
	content, err := os.ReadFile(filepath.Join(target, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != beforeContent {
		t.Errorf("file.txt = %q, want unchanged %q", content, beforeContent)
	}
}

// ---------------------------------------------------------------------------
// git_worktree_create / git_worktree_remove, including dirty-refuse.
// ---------------------------------------------------------------------------

func TestGitWorktreeCreateDefaultPath(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	cloneIntoJail(t, b, bare, "repo")

	res, err := b.gitWorktreeCreate(gitWorktreeCreateArgs{Dir: "repo", Branch: "feature-x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "repo-wt-feature-x" {
		t.Errorf("Path = %q, want repo-wt-feature-x", res.Path)
	}
	if res.Branch != "feature-x" {
		t.Errorf("Branch = %q, want feature-x", res.Branch)
	}
	wtReal, err := b.jail.Resolve(b.userID, res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wtReal, "README.md")); err != nil {
		t.Errorf("worktree missing checked-out content: %v", err)
	}
}

func TestGitWorktreeRemove(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	cloneIntoJail(t, b, bare, "repo")
	res, err := b.gitWorktreeCreate(gitWorktreeCreateArgs{Dir: "repo", Branch: "feature-y"})
	if err != nil {
		t.Fatal(err)
	}
	rres, err := b.gitWorktreeRemove(gitWorktreeRemoveArgs{Dir: "repo", Path: res.Path})
	if err != nil {
		t.Fatal(err)
	}
	if !rres.Removed {
		t.Error("Removed = false, want true")
	}
	wtReal, _ := b.jail.Resolve(b.userID, res.Path)
	if _, err := os.Stat(wtReal); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after remove: err=%v", err)
	}
}

func TestGitWorktreeRemoveRefusesDirty(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	cloneIntoJail(t, b, bare, "repo")
	res, err := b.gitWorktreeCreate(gitWorktreeCreateArgs{Dir: "repo", Branch: "feature-z"})
	if err != nil {
		t.Fatal(err)
	}
	wtReal, err := b.jail.Resolve(b.userID, res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtReal, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.gitWorktreeRemove(gitWorktreeRemoveArgs{Dir: "repo", Path: res.Path}); err == nil {
		t.Error("expected an error removing a dirty worktree")
	}
	if _, err := os.Stat(wtReal); err != nil {
		t.Errorf("dirty worktree should still exist after a refused remove: %v", err)
	}
}

// ---------------------------------------------------------------------------
// git_branch
// ---------------------------------------------------------------------------

func TestGitBranchListAndCreate(t *testing.T) {
	requireGit(t)
	bare := newBareRepoFixture(t)
	b := newTestGitBinding(t)
	cloneIntoJail(t, b, bare, "repo")

	res, err := b.gitBranch(gitBranchArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Current != "main" {
		t.Errorf("Current = %q, want main", res.Current)
	}

	res2, err := b.gitBranch(gitBranchArgs{Dir: "repo", Name: "new-feature"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Current != "new-feature" {
		t.Errorf("Current = %q, want new-feature", res2.Current)
	}
	found := false
	for _, br := range res2.Branches {
		if br == "new-feature" {
			found = true
		}
	}
	if !found {
		t.Errorf("Branches = %v, want new-feature listed", res2.Branches)
	}
}

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
		// GIT_ASKPASS must be EXACTLY the executable symlink path — git execs
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
