// Package pluginregtest is the shared git-fixture harness: a local "remote"
// repo standing in for github.com, no network. Does NOT import pluginreg -
// an internal pluginreg *_test.go file importing this would cycle back.
package pluginregtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RunGit runs a git command against dir, failing the test on error.
func RunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// NewFixtureRepo makes a bare "remote" repo plus a pushing work tree (both
// under t.TempDir()), with one committed file (SKILL.md) and a "v1" tag.
func NewFixtureRepo(t *testing.T) (bare, work string) {
	t.Helper()
	bare = filepath.Join(t.TempDir(), "remote.git")
	RunGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)

	work = t.TempDir()
	RunGit(t, work, "init", "--quiet", "--initial-branch=main")
	RunGit(t, work, "config", "user.email", "test@example.com")
	RunGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "SKILL.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	RunGit(t, work, "add", ".")
	RunGit(t, work, "commit", "--quiet", "-m", "v1")
	RunGit(t, work, "remote", "add", "origin", bare)
	RunGit(t, work, "push", "--quiet", "origin", "main")
	RunGit(t, work, "tag", "v1")
	RunGit(t, work, "push", "--quiet", "origin", "v1")
	return bare, work
}

// CommitAndPush writes content to work's SKILL.md, commits and pushes it,
// and returns the new commit's sha.
func CommitAndPush(t *testing.T, work, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	RunGit(t, work, "add", ".")
	RunGit(t, work, "commit", "--quiet", "-m", content)
	RunGit(t, work, "push", "--quiet", "origin", "main")
	return strings.TrimSpace(RunGit(t, work, "rev-parse", "HEAD"))
}
