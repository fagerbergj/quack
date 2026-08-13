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

	block := environmentBlock(context.Background(), dir, workspace.DefaultCaps())

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
	block := environmentBlock(context.Background(), dir, workspace.DefaultCaps())
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
	block := environmentBlock(context.Background(), dir, workspace.DefaultCaps())
	if !strings.Contains(block, "first "+strconv.Itoa(maxEnvironmentEntries)) {
		t.Errorf("block does not note truncation: %q", block)
	}
}

// TestEnvironmentBlockEmptyDir: an existing but empty cwd reports "(none)"
// rather than an empty, ambiguous line.
func TestEnvironmentBlockEmptyDir(t *testing.T) {
	dir := t.TempDir()
	block := environmentBlock(context.Background(), dir, workspace.DefaultCaps())
	if !strings.Contains(block, "entries: (none") {
		t.Errorf("block = %q, want an explicit empty-entries line", block)
	}
}

// TestEnvironmentBlockDisclosesReadOnly: a read-only round's block states the
// filesystem is read-only (the landlock grant already enforces it - a
// reviewer burning a round on an unexplained npm install EACCES is the bug
// this line fixes), and stays silent when the tree is writable.
func TestEnvironmentBlockDisclosesReadOnly(t *testing.T) {
	dir := t.TempDir()

	caps := workspace.DefaultCaps()
	caps.ReadOnly = true
	block := environmentBlock(context.Background(), dir, caps)
	if !strings.Contains(block, "filesystem: this working tree is READ-ONLY") {
		t.Errorf("block = %q, want a read-only filesystem disclosure line", block)
	}
	if !strings.Contains(block, "EACCES") {
		t.Errorf("block = %q, want the EACCES consequence named", block)
	}

	block = environmentBlock(context.Background(), dir, workspace.DefaultCaps())
	if strings.Contains(block, "filesystem:") {
		t.Errorf("block = %q, want no filesystem line for a writable tree", block)
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
