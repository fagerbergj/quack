package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func gitConfigFixture(t *testing.T) (bin, dir string) {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not found on PATH")
	}
	dir = t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.name", "q"},
		{"config", "url.file:///decoy.insteadOf", "https://example.com/"},
		{"config", "include.path", "/nonexistent"},
		{"config", "http.proxy", "http://127.0.0.1:9"},
	} {
		if out, err := exec.Command(bin, append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return bin, dir
}

func runGitCmd(t *testing.T, bin, dir string, argv ...string) (string, error) {
	t.Helper()
	return runGitCmdIn(t, bin, dir, dir, argv...)
}

func runGitCmdIn(t *testing.T, bin, clone, dir string, argv ...string) (string, error) {
	t.Helper()
	cmd, done, err := GitCmd(context.Background(), bin, clone, dir, argv, nil)
	if err != nil {
		return "", err
	}
	defer done()
	out, err := cmd.Output()
	return string(out), err
}

// TestGitCmdStripsRepoConfigInRepo: every in-repo call keeps gitConfigKeep and drops the rest.
func TestGitCmdStripsRepoConfigInRepo(t *testing.T) {
	bin, dir := gitConfigFixture(t)
	out, err := runGitCmd(t, bin, dir, "config", "--local", "--list")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"url.", "include.", "http."} {
		if strings.Contains(out, gone) {
			t.Errorf("in-repo call kept %s* in repo config:\n%s", gone, out)
		}
	}
	if !strings.Contains(out, "user.name=q") {
		t.Errorf("in-repo call dropped an allowlisted key:\n%s", out)
	}
}

// TestGitCmdHomeIsEmptyAndRemoved: dir "" runs in the per-call HOME, which starts empty and is removed after.
func TestGitCmdHomeIsEmptyAndRemoved(t *testing.T) {
	bin, _ := gitConfigFixture(t)
	cmd, done, err := GitCmd(context.Background(), bin, "", "", []string{"version"}, []string{"HOME=/agent/home"})
	if err != nil {
		t.Fatal(err)
	}
	want := "\nGIT_CONFIG_NOSYSTEM=1\nGIT_CONFIG_GLOBAL=/dev/null\nGIT_TERMINAL_PROMPT=0\nGIT_CEILING_DIRECTORIES=" + filepath.Dir(cmd.Dir)
	if !strings.HasSuffix(strings.Join(cmd.Env, "\n"), want) {
		t.Errorf("env does not end with the pinned HOME/config sources: %v", cmd.Env)
	}
	if entries, err := os.ReadDir(cmd.Dir); err != nil || len(entries) != 0 {
		t.Errorf("HOME %s not an empty dir: %v %v", cmd.Dir, entries, err)
	}
	done()
	if _, err := os.Stat(cmd.Dir); !os.IsNotExist(err) {
		t.Errorf("HOME %s survived done(): %v", cmd.Dir, err)
	}
}

// TestGitCmdRefusesUnreadableRepoConfig: an in-repo call fails closed outside a repo or on a symlinked config.
func TestGitCmdRefusesUnreadableRepoConfig(t *testing.T) {
	bin, dir := gitConfigFixture(t)
	if _, err := runGitCmd(t, bin, t.TempDir(), "version"); err == nil {
		t.Error("call outside a repo: want an error")
	}
	cfg := filepath.Join(dir, ".git", "config")
	moved := filepath.Join(t.TempDir(), "config")
	if err := os.Rename(cfg, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runGitCmd(t, bin, dir, "version"); err == nil {
		t.Error("call on a symlinked repo config: want an error")
	}
}

// TestGitCmdNeverStripsEnclosingRepo: a dir whose .git is broken fails closed instead of stripping its parent repo's config.
func TestGitCmdNeverStripsEnclosingRepo(t *testing.T) {
	bin, dir := gitConfigFixture(t)
	nested := filepath.Join(dir, "plugins", "x", "repo")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runGitCmd(t, bin, nested, "version"); err == nil {
		t.Error("call in a dir with a broken .git: want an error")
	}
	out, err := exec.Command(bin, "-C", dir, "config", "--local", "http.proxy").Output()
	if err != nil || strings.TrimSpace(string(out)) != "http://127.0.0.1:9" {
		t.Errorf("enclosing repo config stripped: %q %v", out, err)
	}
}

// TestGitCmdResolvesSymlinkedDir: a symlink to a dir with a broken .git fails closed instead of reaching the
// enclosing repo.
func TestGitCmdResolvesSymlinkedDir(t *testing.T) {
	bin, dir := gitConfigFixture(t)
	nested := filepath.Join(dir, "sub", "repo")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}
	if _, err := runGitCmd(t, bin, link, "version"); err == nil {
		t.Error("call via a symlink to a broken .git: want an error")
	}
	out, err := exec.Command(bin, "-C", dir, "config", "--local", "http.proxy").Output()
	if err != nil || strings.TrimSpace(string(out)) != "http://127.0.0.1:9" {
		t.Errorf("enclosing repo config stripped: %q %v", out, err)
	}
}

// TestGitCmdRefusesListSeparatorInPath: a ':' in the empty HOME's parent would split GIT_CEILING_DIRECTORIES.
func TestGitCmdRefusesListSeparatorInPath(t *testing.T) {
	bin, _ := gitConfigFixture(t)
	tmp := filepath.Join(t.TempDir(), "a"+string(os.PathListSeparator)+"b")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	if _, err := runGitCmd(t, bin, "", "version"); err == nil || !strings.Contains(err.Error(), "GIT_CEILING_DIRECTORIES") {
		t.Errorf("err = %v, want a refusal naming GIT_CEILING_DIRECTORIES", err)
	}
}

// repoWithWorktree: gitConfigFixture plus one commit and a linked worktree of it.
func repoWithWorktree(t *testing.T) (bin, clone, wt string) {
	t.Helper()
	bin, clone = gitConfigFixture(t)
	wt = filepath.Join(t.TempDir(), "wt")
	for _, args := range [][]string{
		{"-c", "user.email=q@x.local", "commit", "--quiet", "--allow-empty", "-m", "init"},
		{"worktree", "add", "--quiet", "--detach", wt},
	} {
		if out, err := exec.Command(bin, append([]string{"-C", clone}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return bin, clone, wt
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGitCmdRefusesRepoRedirects: a writable tree can't aim quack's git at another repo; the other repo's config
// (which an in-repo call would strip) survives untouched.
func TestGitCmdRefusesRepoRedirects(t *testing.T) {
	for name, redirect := range map[string]func(t *testing.T, clone, wt, other string) (dir string){
		"dot git symlink": func(t *testing.T, clone, _, other string) string {
			if err := os.RemoveAll(filepath.Join(clone, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(other, ".git"), filepath.Join(clone, ".git")); err != nil {
				t.Fatal(err)
			}
			return clone
		},
		"dot git file in clone": func(t *testing.T, clone, _, other string) string {
			if err := os.RemoveAll(filepath.Join(clone, ".git")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(clone, ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n")
			return clone
		},
		"worktree gitdir outside clone": func(t *testing.T, _, wt, other string) string {
			writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n")
			return wt
		},
		"worktree commondir elsewhere": func(t *testing.T, clone, wt, other string) string {
			writeFile(t, filepath.Join(clone, ".git", "worktrees", "wt", "commondir"), filepath.Join(other, ".git")+"\n")
			return wt
		},
		"worktree gitdir outside worktrees dir": func(t *testing.T, clone, wt, _ string) string {
			fake := filepath.Join(t.TempDir(), "wt")
			if err := os.Rename(filepath.Join(clone, ".git", "worktrees", "wt"), fake); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(fake, "commondir"), filepath.Join(clone, ".git")+"\n")
			writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+fake+"\n")
			return wt
		},
		"worktree pointer is a fifo": func(t *testing.T, _, wt, _ string) string {
			if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(wt, ".git"), 0o644); err != nil {
				t.Fatal(err)
			}
			return wt
		},
		"clone commondir": func(t *testing.T, clone, _, other string) string {
			writeFile(t, filepath.Join(clone, ".git", "commondir"), filepath.Join(other, ".git")+"\n")
			return clone
		},
		"alternates": func(t *testing.T, clone, _, other string) string {
			writeFile(t, filepath.Join(clone, ".git", "objects", "info", "alternates"), filepath.Join(other, ".git", "objects")+"\n")
			return clone
		},
		"http alternates": func(t *testing.T, clone, _, _ string) string {
			writeFile(t, filepath.Join(clone, ".git", "objects", "info", "http-alternates"), "https://example.com/objects\n")
			return clone
		},
	} {
		t.Run(name, func(t *testing.T) {
			bin, clone, wt := repoWithWorktree(t)
			_, other := gitConfigFixture(t)
			dir := redirect(t, clone, wt, other)
			if _, err := runGitCmdIn(t, bin, clone, dir, "config", "--local", "--list"); err == nil || !strings.Contains(err.Error(), "not a repository quack created") {
				t.Errorf("err = %v, want a refusal", err)
			}
			out, err := exec.Command(bin, "-C", other, "config", "--local", "http.proxy").Output()
			if err != nil || strings.TrimSpace(string(out)) != "http://127.0.0.1:9" {
				t.Errorf("other repo's config was touched: %q %v", out, err)
			}
		})
	}
}

// TestGitCmdPinsLegitimateRepos: the clone itself and its own linked worktree still run, each on its own gitdir.
func TestGitCmdPinsLegitimateRepos(t *testing.T) {
	bin, clone, wt := repoWithWorktree(t)
	real, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		clone: filepath.Join(real, ".git"),
		wt:    filepath.Join(real, ".git", "worktrees", "wt"),
	} {
		out, err := runGitCmdIn(t, bin, clone, dir, "rev-parse", "--absolute-git-dir")
		if err != nil || strings.TrimSpace(out) != want {
			t.Errorf("git dir for %s = %q %v, want %s", dir, out, err, want)
		}
	}
}

// TestWorktreeCloneRefusesOutsideRoot: a worktree whose clone lies outside root yields no clone to prune in.
func TestWorktreeCloneRefusesOutsideRoot(t *testing.T) {
	_, clone, wt := repoWithWorktree(t)
	if got, err := WorktreeClone(t.TempDir(), wt); err == nil {
		t.Errorf("WorktreeClone outside root = %q, want an error", got)
	}
	got, err := WorktreeClone(filepath.Dir(clone), wt)
	real, _ := filepath.EvalSymlinks(clone)
	if err != nil || got != real {
		t.Errorf("WorktreeClone inside root = %q %v, want %s", got, err, real)
	}
	if got, err := WorktreeClone(filepath.Dir(clone), clone); got != "" || err != nil {
		t.Errorf("WorktreeClone(plain clone) = %q %v, want \"\" nil", got, err)
	}
}

// TestGitCmdPinsRepoAgainstLaterSwap: redirects written after validation still don't move the built command.
func TestGitCmdPinsRepoAgainstLaterSwap(t *testing.T) {
	bin, clone, wt := repoWithWorktree(t)
	_, other := gitConfigFixture(t)
	cmd, done, err := GitCmd(context.Background(), bin, clone, wt, []string{"rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n")
	writeFile(t, filepath.Join(clone, ".git", "worktrees", "wt", "commondir"), filepath.Join(other, ".git")+"\n")
	out, err := cmd.Output()
	real, _ := filepath.EvalSymlinks(clone)
	want := filepath.Join(real, ".git", "worktrees", "wt") + "\n" + filepath.Join(real, ".git") + "\n"
	if err != nil || string(out) != want {
		t.Errorf("git dirs = %q %v, want %q", out, err, want)
	}
}
