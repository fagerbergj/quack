package pluginreg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
)

// newFixtureRepoWithSkill is pluginregtest.NewFixtureRepo's skill-shaped
// twin: skills/dothing/SKILL.md instead of a root-level SKILL.md, so TreeAt
// round-trips through skill.NewFileSystemSource in skillsource_test.go too.
func newFixtureRepoWithSkill(t *testing.T, body string) (bare, work string) {
	t.Helper()
	bare, work = pluginregtest.NewFixtureRepo(t)
	run(t, work, "rm", "--quiet", "SKILL.md")
	writeSkillCommit(t, work, body)
	return bare, work
}

// writeSkillCommit (re)writes skills/dothing/SKILL.md, commits and pushes,
// and returns the new commit's sha.
func writeSkillCommit(t *testing.T, work, body string) string {
	t.Helper()
	dir := filepath.Join(work, "skills", "dothing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "skill update")
	run(t, work, "push", "--quiet", "origin", "main")
	return strings.TrimSpace(run(t, work, "rev-parse", "HEAD"))
}

func TestTreeAt_MissingCloneOrUnknownSHARefuses(t *testing.T) {
	bare, _ := newFixtureRepoWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	got, err := reg.Fetch(context.Background(), FromEntry(mustParse(t, "github:acme/widgets")))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := TreeAt(context.Background(), root, "widgets", strings.Repeat("a", 40), "skills"); err == nil {
		t.Fatal("TreeAt(unknown sha): want error")
	}
	if _, err := TreeAt(context.Background(), root, "no-such-plugin", got.SHA, "skills"); err == nil {
		t.Fatal("TreeAt(no clone): want error")
	}
}

func TestTreeAt(t *testing.T) {
	bare, work := newFixtureRepoWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	p := FromEntry(mustParse(t, "github:acme/widgets"))
	sha1, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	sha2 := writeSkillCommit(t, work, "---\nname: dothing\ndescription: v2\n---\nbody v2")
	if _, err := reg.Fetch(context.Background(), sha1); err != nil {
		t.Fatal(err)
	}

	oldFS, err := TreeAt(context.Background(), root, "widgets", sha1.SHA, "skills")
	if err != nil {
		t.Fatalf("TreeAt(sha1): %v", err)
	}
	oldBody, err := oldFS.ReadFile("dothing/SKILL.md")
	if err != nil {
		t.Fatalf("read old SKILL.md: %v", err)
	}
	if !strings.Contains(string(oldBody), "body v1") {
		t.Fatalf("old body = %q, want containing %q", oldBody, "body v1")
	}

	newFS, err := TreeAt(context.Background(), root, "widgets", sha2, "skills")
	if err != nil {
		t.Fatalf("TreeAt(sha2): %v", err)
	}
	newBody, err := newFS.ReadFile("dothing/SKILL.md")
	if err != nil {
		t.Fatalf("read new SKILL.md: %v", err)
	}
	if !strings.Contains(string(newBody), "body v2") {
		t.Fatalf("new body = %q, want containing %q", newBody, "body v2")
	}

	// A subdir absent at the sha is an empty tree, not an error.
	empty, err := TreeAt(context.Background(), root, "widgets", sha1.SHA, "no-such-dir")
	if err != nil {
		t.Fatalf("TreeAt(missing subdir): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("TreeAt(missing subdir) = %v entries, want 0", len(empty))
	}
}

// TestTreeAt_NonASCIIPath is review S2: core.quotePath C-quotes a non-ASCII
// path in `ls-tree`'s plain output, breaking the `git show` that follows -
// -z/NUL-split must round-trip the path as written.
func TestTreeAt_NonASCIIPath(t *testing.T) {
	bare, work := pluginregtest.NewFixtureRepo(t)
	run(t, work, "rm", "--quiet", "SKILL.md")
	dir := filepath.Join(work, "skills", "café")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: cafe\ndescription: unicode\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "unicode skill dir")
	run(t, work, "push", "--quiet", "origin", "main")
	sha := strings.TrimSpace(run(t, work, "rev-parse", "HEAD"))

	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	if _, err := reg.Fetch(context.Background(), FromEntry(mustParse(t, "github:acme/widgets"))); err != nil {
		t.Fatal(err)
	}

	fs, err := TreeAt(context.Background(), root, "widgets", sha, "skills")
	if err != nil {
		t.Fatalf("TreeAt: %v", err)
	}
	body, err := fs.ReadFile("café/SKILL.md")
	if err != nil {
		t.Fatalf("read café/SKILL.md: %v", err)
	}
	if !strings.Contains(string(body), "body") {
		t.Fatalf("body = %q", body)
	}
}

// TestTreeAt_EscapingSubdirFallsBackToSkills is the #1446 carry-over: a
// subdir that escapes the clone (a row trusted off disk without
// re-parsing, same as Plugin.Root) falls back to "skills" at the clone
// root instead, so replay matches live's own fallback.
func TestTreeAt_EscapingSubdirFallsBackToSkills(t *testing.T) {
	bare, _ := newFixtureRepoWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	got, err := reg.Fetch(context.Background(), FromEntry(mustParse(t, "github:acme/widgets")))
	if err != nil {
		t.Fatal(err)
	}

	fs, err := TreeAt(context.Background(), root, "widgets", got.SHA, "../../../etc/skills")
	if err != nil {
		t.Fatalf("TreeAt(escaping subdir): %v", err)
	}
	if _, err := fs.ReadFile("dothing/SKILL.md"); err != nil {
		t.Fatalf("TreeAt(escaping subdir) did not fall back to \"skills\" at the clone root: %v", err)
	}
}

func mustParse(t *testing.T, s string) Entry {
	t.Helper()
	e, err := ParseEntry(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
