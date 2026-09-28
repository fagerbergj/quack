package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestEnvironmentBlockShape pins the environment block's shape: absolute cwd, whether it's a
// git repo (with branch/HEAD when so), and the top-level entries - all
// wrapped in a factual <environment_context> block, never an instruction.
func TestEnvironmentBlockShape(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "quack/work")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "-m", "init")

	block, _ := environmentBlock(context.Background(), nil, dir, workspace.DefaultCaps())

	if !strings.HasPrefix(block, "<environment_context>\n") || !strings.HasSuffix(block, "</environment_context>") {
		t.Fatalf("block is not wrapped in <environment_context>: %q", block)
	}
	if !strings.Contains(block, "cwd: "+dir) {
		t.Errorf("block missing the absolute cwd: %q", block)
	}
	if !strings.Contains(block, "git: yes (branch quack/work, HEAD ") {
		t.Errorf("block missing git branch/HEAD: %q", block)
	}
	if !strings.Contains(block, "README.md") || !strings.Contains(block, "internal/") {
		t.Errorf("block missing top-level entries (file and dir/): %q", block)
	}
}

// TestEnvironmentBlockNonRepo: a plain (non-git) working directory reports
// "git: no" rather than failing or fabricating branch info.
func TestEnvironmentBlockNonRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	block, _ := environmentBlock(context.Background(), nil, dir, workspace.DefaultCaps())
	if !strings.Contains(block, "git: no") {
		t.Errorf("block = %q, want \"git: no\" for a non-repo cwd", block)
	}
	if !strings.Contains(block, "notes.txt") {
		t.Errorf("block missing the plain file entry: %q", block)
	}
}

// TestEnvironmentBlockBoundsEntries pins the "a pathological dir must not
// blow the context window" requirement: entries beyond maxEnvironmentEntries
// are dropped, and the block says so, rather than growing without bound.
func TestEnvironmentBlockBoundsEntries(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxEnvironmentEntries+50; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%04d.txt", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, truncated := topLevelEntries(dir)
	if !truncated {
		t.Fatal("topLevelEntries: want truncated=true past the bound")
	}
	if len(entries) != maxEnvironmentEntries {
		t.Errorf("topLevelEntries returned %d entries, want exactly %d", len(entries), maxEnvironmentEntries)
	}
	block, _ := environmentBlock(context.Background(), nil, dir, workspace.DefaultCaps())
	if !strings.Contains(block, "first "+strconv.Itoa(maxEnvironmentEntries)) {
		t.Errorf("block does not note truncation: %q", block)
	}
}

// TestEnvironmentBlockEmptyDir: an existing but empty cwd reports "(none)"
// rather than an empty, ambiguous line.
func TestEnvironmentBlockEmptyDir(t *testing.T) {
	dir := t.TempDir()
	block, _ := environmentBlock(context.Background(), nil, dir, workspace.DefaultCaps())
	if !strings.Contains(block, "entries: (none") {
		t.Errorf("block = %q, want an explicit empty-entries line", block)
	}
}

// TestEnvironmentBlockDisclosesReadOnly: a read-only round's block names both
// sides - which path is read-only and which are writable. Naming only the
// read-only half is what left reviewers either burning a round on an unexplained EACCES or abandoning "run it" entirely. Stays silent when the tree is writable.
func TestEnvironmentBlockDisclosesReadOnly(t *testing.T) {
	dir := t.TempDir()

	caps := workspace.DefaultCaps()
	caps.ReadOnly = true
	block, _ := environmentBlock(context.Background(), nil, dir, caps)
	if !strings.Contains(block, "filesystem: read-only") || !strings.Contains(block, dir) {
		t.Errorf("block = %q, want the read-only path named", block)
	}
	if !strings.Contains(block, "filesystem: writable: ") {
		t.Errorf("block = %q, want the writable paths named - the half that makes running the change possible", block)
	}
	if !strings.Contains(block, workspace.SandboxTmpDir(caps)) {
		t.Errorf("block = %q, want the scratch dir named as writable", block)
	}
	if !strings.Contains(block, "EACCES") {
		t.Errorf("block = %q, want the EACCES consequence named", block)
	}

	block, _ = environmentBlock(context.Background(), nil, dir, workspace.DefaultCaps())
	if strings.Contains(block, "filesystem:") {
		t.Errorf("block = %q, want no filesystem line for a writable tree", block)
	}
}

func gitInfoFixture(t *testing.T) (git, dir string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir = t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "quack/work")
	runGit(t, dir, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "init")
	return git, dir
}

// TestGitInfoRunsInRoundSandbox: the probe honours the round's sandbox and reports the same facts as unsandboxed.
func TestGitInfoRunsInRoundSandbox(t *testing.T) {
	_, dir := gitInfoFixture(t)
	caps := workspace.DefaultCaps()
	caps.Sandbox, caps.HomeDir = workspace.SandboxBwrap, t.TempDir()
	if res, err := workspace.RunArgv(context.Background(), dir, []string{"true"}, caps); err != nil || res.ExitCode != 0 {
		t.Skipf("SKIPPING: bubblewrap is not usable here (%v, %q)", err, res.Output)
	}
	branch, sha, ok := gitInfo(context.Background(), dir, caps)
	wantBranch, wantSha, wantOK := gitInfo(context.Background(), dir, workspace.DefaultCaps())
	if !ok || branch != "quack/work" || sha == "" || branch != wantBranch || sha != wantSha || ok != wantOK {
		t.Fatalf("sandboxed gitInfo = (%q, %q, %v), unsandboxed = (%q, %q, %v)", branch, sha, ok, wantBranch, wantSha, wantOK)
	}
}

// TestGitInfoOmittedWhenSandboxUnavailable: an unreachable sandbox binary drops the git line, never runs git bare.
func TestGitInfoOmittedWhenSandboxUnavailable(t *testing.T) {
	git, dir := gitInfoFixture(t)
	fakePath := t.TempDir() // nothing but git: bwrap unresolvable
	if err := os.Symlink(git, filepath.Join(fakePath, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakePath)

	if branch, sha, ok := gitInfo(context.Background(), dir, workspace.Caps{Sandbox: workspace.SandboxBwrap}); ok {
		t.Fatalf("gitInfo(%q) = (%q, %q, true), want ok=false with bwrap unreachable", dir, branch, sha)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
