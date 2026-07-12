package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestUserIDValidation guards the jail boundary against attacker-influenced
// identities (an OIDC subject is external text): a userID containing a
// separator or dot-traversal would relocate the jail root itself, making the
// containment check verify against the WRONG root. Both entry points
// (UserRoot and Resolve) must reject identically with ErrInvalidUserID — a
// distinct error from ErrEscape, since this is a caller/config bug, not a
// model-chosen path. Real OIDC-shaped ids (pipes, emails) must PASS: the rule
// is separator/dot-traversal based, not an alphanumeric allowlist.
func TestUserIDValidation(t *testing.T) {
	j := newTestJail(t)
	for _, tc := range []struct {
		userID string
		valid  bool
	}{
		{"", false},
		{"   ", false},
		{".", false},
		{"..", false},
		{"../bob", false},
		{"..%2Fbob", true}, // encoded separators are literal text at this layer — a legal (odd) dirname
		{"a/b", false},
		{"/etc", false},
		{"/", false},
		{"local", true},
		{"auth0|abc123", true},
		{"user@example.com", true},
		{"S-1-5-21-1004", true},
	} {
		_, uerr := j.UserRoot(tc.userID)
		_, rerr := j.Resolve(tc.userID, "f.txt")
		if tc.valid {
			if uerr != nil {
				t.Errorf("UserRoot(%q) = %v, want accepted", tc.userID, uerr)
			}
			if rerr != nil {
				t.Errorf("Resolve(%q, f.txt) = %v, want accepted", tc.userID, rerr)
			}
			continue
		}
		if !errors.Is(uerr, ErrInvalidUserID) {
			t.Errorf("UserRoot(%q) err = %v, want ErrInvalidUserID", tc.userID, uerr)
		}
		if !errors.Is(rerr, ErrInvalidUserID) {
			t.Errorf("Resolve(%q, f.txt) err = %v, want ErrInvalidUserID", tc.userID, rerr)
		}
	}
}

// TestBadUserIDIsNotErrEscape pins the error-identity contract: a bad userID
// must never read as a jail escape (the model can't cause or fix ErrEscape's
// "path escapes your workspace"; an invalid identity is the operator's bug).
func TestBadUserIDIsNotErrEscape(t *testing.T) {
	j := newTestJail(t)
	_, err := j.Resolve("../bob", "f.txt")
	if errors.Is(err, ErrEscape) {
		t.Fatalf("Resolve with a bad userID returned ErrEscape; want the distinct ErrInvalidUserID")
	}
	if !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("err = %v, want ErrInvalidUserID", err)
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

func TestJailHomeDirIsSiblingNotNestedInARepo(t *testing.T) {
	j := newTestJail(t)
	// Simulate a cloned repo living directly under the user's jail root, the
	// way git_clone's default `dir` (the repo name) does.
	repoDir, err := j.Resolve("alice", "myrepo")
	if err != nil {
		t.Fatalf("Resolve(myrepo): %v", err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	home, err := j.HomeDir("alice")
	if err != nil {
		t.Fatalf("HomeDir: %v", err)
	}
	if strings.HasPrefix(home, repoDir+string(filepath.Separator)) || home == repoDir {
		t.Fatalf("HomeDir %q is nested inside the cloned repo %q — must be a sibling", home, repoDir)
	}
	userRoot, err := j.UserRoot("alice")
	if err != nil {
		t.Fatalf("UserRoot: %v", err)
	}
	if !strings.HasPrefix(home, userRoot+string(filepath.Separator)) {
		t.Errorf("HomeDir %q is not under the user's own jail root %q", home, userRoot)
	}
}

func TestJailHomeDirCreatesDirectory(t *testing.T) {
	j := newTestJail(t)
	home, err := j.HomeDir("alice")
	if err != nil {
		t.Fatalf("HomeDir: %v", err)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("HomeDir did not create %q: %v", home, err)
	}
	if !info.IsDir() {
		t.Errorf("HomeDir %q is not a directory", home)
	}
}

func TestJailHomeDirIsStableAcrossCalls(t *testing.T) {
	j := newTestJail(t)
	home1, err := j.HomeDir("alice")
	if err != nil {
		t.Fatalf("HomeDir: %v", err)
	}
	home2, err := j.HomeDir("alice")
	if err != nil {
		t.Fatalf("HomeDir (2nd call): %v", err)
	}
	if home1 != home2 {
		t.Errorf("HomeDir not stable: %q vs %q", home1, home2)
	}
}

func TestJailHomeDirPerUserIsolated(t *testing.T) {
	j := newTestJail(t)
	alice, err := j.HomeDir("alice")
	if err != nil {
		t.Fatalf("HomeDir(alice): %v", err)
	}
	bob, err := j.HomeDir("bob")
	if err != nil {
		t.Fatalf("HomeDir(bob): %v", err)
	}
	if alice == bob {
		t.Errorf("HomeDir(alice) == HomeDir(bob) = %q, want distinct per-user homes", alice)
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
