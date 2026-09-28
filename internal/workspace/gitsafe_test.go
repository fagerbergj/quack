package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func runGitCmd(t *testing.T, bin, dir string, credentialed bool, argv ...string) (string, error) {
	t.Helper()
	cmd, done, err := GitCmd(context.Background(), bin, dir, argv, nil, credentialed)
	if err != nil {
		return "", err
	}
	defer done()
	out, err := cmd.Output()
	return string(out), err
}

// TestGitCmdStripsRepoConfigOnlyWhenCredentialed: a credentialed call keeps gitConfigKeep and drops the rest.
func TestGitCmdStripsRepoConfigOnlyWhenCredentialed(t *testing.T) {
	bin, dir := gitConfigFixture(t)
	out, err := runGitCmd(t, bin, dir, false, "config", "--local", "--list")
	if err != nil || !strings.Contains(out, "url.file:///decoy.insteadof") {
		t.Fatalf("uncredentialed call changed repo config: %q (err=%v)", out, err)
	}
	out, err = runGitCmd(t, bin, dir, true, "config", "--local", "--list")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"url.", "include.", "http."} {
		if strings.Contains(out, gone) {
			t.Errorf("credentialed call kept %s* in repo config:\n%s", gone, out)
		}
	}
	if !strings.Contains(out, "user.name=q") {
		t.Errorf("credentialed call dropped an allowlisted key:\n%s", out)
	}
}

// TestGitCmdHomeIsEmptyAndRemoved: dir "" runs in the per-call HOME, which starts empty and is removed after.
func TestGitCmdHomeIsEmptyAndRemoved(t *testing.T) {
	bin, _ := gitConfigFixture(t)
	cmd, done, err := GitCmd(context.Background(), bin, "", []string{"version"}, []string{"HOME=/agent/home"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.Join(cmd.Env, "\n"), "HOME="+cmd.Dir+"\nGIT_CONFIG_NOSYSTEM=1\nGIT_CONFIG_GLOBAL=/dev/null\nGIT_TERMINAL_PROMPT=0") {
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

// TestGitCmdRefusesUnreadableRepoConfig: a credentialed call fails closed outside a repo or on a symlinked config.
func TestGitCmdRefusesUnreadableRepoConfig(t *testing.T) {
	bin, dir := gitConfigFixture(t)
	if _, err := runGitCmd(t, bin, t.TempDir(), true, "version"); err == nil {
		t.Error("credentialed call outside a repo: want an error")
	}
	cfg := filepath.Join(dir, ".git", "config")
	moved := filepath.Join(t.TempDir(), "config")
	if err := os.Rename(cfg, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runGitCmd(t, bin, dir, true, "version"); err == nil {
		t.Error("credentialed call on a symlinked repo config: want an error")
	}
}
