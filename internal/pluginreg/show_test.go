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

func TestVerifyCommit(t *testing.T) {
	bare, work := newFixtureRepoWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	p := FromEntry(mustParse(t, "github:acme/widgets"))
	got, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	sha2 := writeSkillCommit(t, work, "---\nname: dothing\ndescription: v2\n---\nbody v2")
	if _, err := reg.Fetch(context.Background(), got); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}

	if err := VerifyCommit(context.Background(), root, "widgets", got.SHA); err != nil {
		t.Fatalf("VerifyCommit(old sha): %v", err)
	}
	if err := VerifyCommit(context.Background(), root, "widgets", sha2); err != nil {
		t.Fatalf("VerifyCommit(new sha): %v", err)
	}
	if err := VerifyCommit(context.Background(), root, "widgets", strings.Repeat("a", 40)); err == nil {
		t.Fatal("VerifyCommit(unknown sha): want error")
	}
	if err := VerifyCommit(context.Background(), root, "no-such-plugin", got.SHA); err == nil {
		t.Fatal("VerifyCommit(no clone): want error")
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

func mustParse(t *testing.T, s string) Entry {
	t.Helper()
	e, err := ParseEntry(s)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
