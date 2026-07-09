package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestJail(t *testing.T) *Jail {
	t.Helper()
	j, err := NewJail(t.TempDir())
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	return j
}

func TestResolveWithinJailWorks(t *testing.T) {
	j := newTestJail(t)
	got, err := j.Resolve("alice", "sub/file.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(j.Root(), "alice", "sub", "file.txt")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveRejectsDotDotEscape(t *testing.T) {
	j := newTestJail(t)
	// Make a sibling directory to escape into, so a naive check that only
	// stats existence wouldn't already fail for an unrelated reason.
	if err := os.MkdirAll(filepath.Join(j.Root(), "bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := j.Resolve("alice", "../bob/secret.txt")
	if !errors.Is(err, ErrEscape) {
		t.Fatalf("Resolve(..escape) err = %v, want ErrEscape", err)
	}
}

func TestResolveRejectsAbsolutePath(t *testing.T) {
	j := newTestJail(t)
	_, err := j.Resolve("alice", "/etc/passwd")
	if !errors.Is(err, ErrEscape) {
		t.Fatalf("Resolve(absolute) err = %v, want ErrEscape", err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	j := newTestJail(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("shh"), 0o644); err != nil {
		t.Fatal(err)
	}
	aliceRoot := filepath.Join(j.Root(), "alice")
	if err := os.MkdirAll(aliceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(aliceRoot, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := j.Resolve("alice", "escape/secret.txt")
	if !errors.Is(err, ErrEscape) {
		t.Fatalf("Resolve(symlink escape) err = %v, want ErrEscape", err)
	}
}

func TestResolveSymlinkInsideJailWorks(t *testing.T) {
	j := newTestJail(t)
	aliceRoot := filepath.Join(j.Root(), "alice")
	target := filepath.Join(aliceRoot, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(aliceRoot, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := j.Resolve("alice", "link/f.txt")
	if err != nil {
		t.Fatalf("Resolve(symlink inside jail): %v", err)
	}
	want := filepath.Join(target, "f.txt")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveTwoUsersAreIndependent(t *testing.T) {
	j := newTestJail(t)
	aPath, err := j.Resolve("alice", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	bPath, err := j.Resolve("bob", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if aPath == bPath {
		t.Fatalf("alice and bob resolved to the same path: %q", aPath)
	}
	if err := os.MkdirAll(filepath.Dir(aPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aPath, []byte("alice's content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bPath); !os.IsNotExist(err) {
		t.Fatalf("bob's notes.txt should not exist after alice wrote hers; stat err = %v", err)
	}
}

func TestResolveRejectsEmptyUserID(t *testing.T) {
	j := newTestJail(t)
	if _, err := j.Resolve("", "f.txt"); err == nil {
		t.Fatal("expected error for empty user id")
	}
}

func TestResolveRootItselfWorks(t *testing.T) {
	j := newTestJail(t)
	got, err := j.Resolve("alice", "")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	want := filepath.Join(j.Root(), "alice")
	if got != want {
		t.Errorf("Resolve(\"\") = %q, want %q", got, want)
	}
	got2, err := j.Resolve("alice", ".")
	if err != nil {
		t.Fatalf("Resolve(\".\"): %v", err)
	}
	if got2 != want {
		t.Errorf("Resolve(\".\") = %q, want %q", got2, want)
	}
}

func TestNewJailCreatesRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	j, err := NewJail(dir)
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}
	if _, err := os.Stat(j.Root()); err != nil {
		t.Fatalf("root not created: %v", err)
	}
}

func TestNewJailRejectsEmptyRoot(t *testing.T) {
	if _, err := NewJail(""); err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestDefaultCaps(t *testing.T) {
	c := DefaultCaps()
	if c.MaxReadBytes != 256*1024 {
		t.Errorf("MaxReadBytes = %d, want 256KiB", c.MaxReadBytes)
	}
	if c.MaxWriteBytes != 2*1024*1024 {
		t.Errorf("MaxWriteBytes = %d, want 2MiB", c.MaxWriteBytes)
	}
	if c.MaxResults != 200 {
		t.Errorf("MaxResults = %d, want 200", c.MaxResults)
	}
	if c.MaxListEntries != 500 {
		t.Errorf("MaxListEntries = %d, want 500", c.MaxListEntries)
	}
	if c.Timeout.Seconds() != 60 {
		t.Errorf("Timeout = %v, want 60s", c.Timeout)
	}
}
