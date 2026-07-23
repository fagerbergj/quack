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
	got, err := j.Resolve("alice", "", "sub/file.txt")
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
	_, err := j.Resolve("alice", "", "../bob/secret.txt")
	if !errors.Is(err, ErrEscape) {
		t.Fatalf("Resolve(..escape) err = %v, want ErrEscape", err)
	}
}

func TestResolveRejectsAbsolutePath(t *testing.T) {
	j := newTestJail(t)
	_, err := j.Resolve("alice", "", "/etc/passwd")
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
	_, err := j.Resolve("alice", "", "escape/secret.txt")
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
	got, err := j.Resolve("alice", "", "link/f.txt")
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
	aPath, err := j.Resolve("alice", "", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	bPath, err := j.Resolve("bob", "", "notes.txt")
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
// (UserRoot and Resolve) must reject identically with ErrInvalidUserID - a
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
		{"..%2Fbob", true}, // encoded separators are literal text at this layer - a legal (odd) dirname
		{"a/b", false},
		{"/etc", false},
		{"/", false},
		{"local", true},
		{"auth0|abc123", true},
		{"user@example.com", true},
		{"S-1-5-21-1004", true},
	} {
		_, uerr := j.UserRoot(tc.userID)
		_, rerr := j.Resolve(tc.userID, "", "f.txt")
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
	_, err := j.Resolve("../bob", "", "f.txt")
	if errors.Is(err, ErrEscape) {
		t.Fatalf("Resolve with a bad userID returned ErrEscape; want the distinct ErrInvalidUserID")
	}
	if !errors.Is(err, ErrInvalidUserID) {
		t.Fatalf("err = %v, want ErrInvalidUserID", err)
	}
}

func TestResolveRootItselfWorks(t *testing.T) {
	j := newTestJail(t)
	got, err := j.Resolve("alice", "", "")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	want := filepath.Join(j.Root(), "alice")
	if got != want {
		t.Errorf("Resolve(\"\") = %q, want %q", got, want)
	}
	got2, err := j.Resolve("alice", "", ".")
	if err != nil {
		t.Fatalf("Resolve(\".\"): %v", err)
	}
	if got2 != want {
		t.Errorf("Resolve(\".\") = %q, want %q", got2, want)
	}
}

// TestResolvePerChatScope pins the per-chat segment: a non-empty chatID scopes
// paths under <root>/<user>/<chat>/, two different chat ids get isolated trees,
// and an empty chatID falls back to the per-user root (backward compatible).
func TestResolvePerChatScope(t *testing.T) {
	j := newTestJail(t)
	got, err := j.Resolve("alice", "chat1", "sub/file.txt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(j.Root(), "alice", "chat1", "sub", "file.txt")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}

	// Two chats of the same user resolve to isolated trees.
	c1, _ := j.Resolve("alice", "chat1", "notes.txt")
	c2, _ := j.Resolve("alice", "chat2", "notes.txt")
	if c1 == c2 {
		t.Fatalf("chat1 and chat2 resolved to the same path: %q", c1)
	}
	if err := os.MkdirAll(filepath.Dir(c1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c1, []byte("chat1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c2); !os.IsNotExist(err) {
		t.Fatalf("chat2's notes.txt should not exist after chat1 wrote hers; stat err = %v", err)
	}

	// Empty chatID falls back to the per-user root (today's behaviour).
	fallback, err := j.Resolve("alice", "", "notes.txt")
	if err != nil {
		t.Fatalf("Resolve(empty chat): %v", err)
	}
	if wantFallback := filepath.Join(j.Root(), "alice", "notes.txt"); fallback != wantFallback {
		t.Errorf("Resolve(empty chat) = %q, want per-user root %q", fallback, wantFallback)
	}
}

// TestChatIDValidation guards the per-chat segment: a chat id is a
// system-generated UUID but treated as untrusted here - a separator or
// dot-traversal must never relocate the scope root and let a path escape the
// user jail. A malicious id is rejected with ErrInvalidChatID (distinct from
// ErrInvalidUserID and ErrEscape); an empty id is NOT an error (it is the
// per-user fallback).
func TestChatIDValidation(t *testing.T) {
	j := newTestJail(t)
	// Make a sibling under alice to prove `../` can't reach it.
	if err := os.MkdirAll(filepath.Join(j.Root(), "alice", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		chatID string
		valid  bool
	}{
		{"", true}, // empty = per-user fallback, not an error
		{"chat-123", true},
		{"550e8400-e29b-41d4-a716-446655440000", true},
		{".", false},
		{"..", false},
		{"../other", false},
		{"a/b", false},
		{"/etc", false},
	} {
		_, err := j.Resolve("alice", tc.chatID, "f.txt")
		if tc.valid {
			if err != nil {
				t.Errorf("Resolve(alice, %q, f.txt) = %v, want accepted", tc.chatID, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidChatID) {
			t.Errorf("Resolve(alice, %q, f.txt) err = %v, want ErrInvalidChatID", tc.chatID, err)
		}
		if errors.Is(err, ErrEscape) {
			t.Errorf("bad chatID %q read as ErrEscape; want the distinct ErrInvalidChatID", tc.chatID)
		}
	}
}

// TestRemoveChatScope covers the deletion-cleanup contract: it removes exactly
// the chat's subtree, a non-existent dir is a clean no-op, an empty chatID is
// rejected (never removes the whole user root), and a crafted id can't escape.
func TestRemoveChatScope(t *testing.T) {
	j := newTestJail(t)
	// Populate two chats plus a per-user sibling that must survive.
	c1, _ := j.Resolve("alice", "chat1", "repo/f.txt")
	c2, _ := j.Resolve("alice", "chat2", "repo/f.txt")
	userSibling, _ := j.Resolve("alice", "", ".quack-home")
	for _, p := range []string{c1, c2} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(userSibling, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := j.RemoveChatScope("alice", "chat1"); err != nil {
		t.Fatalf("RemoveChatScope(chat1): %v", err)
	}
	chat1Root := filepath.Join(j.Root(), "alice", "chat1")
	if _, err := os.Stat(chat1Root); !os.IsNotExist(err) {
		t.Fatalf("chat1 tree still exists after RemoveChatScope: %v", err)
	}
	// chat2 and the per-user sibling are untouched.
	if _, err := os.Stat(c2); err != nil {
		t.Errorf("chat2 tree should survive: %v", err)
	}
	if _, err := os.Stat(userSibling); err != nil {
		t.Errorf("per-user sibling (.quack-home) should survive: %v", err)
	}

	// Removing a non-existent chat scope is a clean no-op.
	if err := j.RemoveChatScope("alice", "chat1"); err != nil {
		t.Errorf("RemoveChatScope on missing dir = %v, want nil (no-op)", err)
	}

	// An empty chatID is rejected - must never remove the whole user root.
	if err := j.RemoveChatScope("alice", ""); !errors.Is(err, ErrInvalidChatID) {
		t.Errorf("RemoveChatScope(empty) = %v, want ErrInvalidChatID", err)
	}
	if _, err := os.Stat(filepath.Join(j.Root(), "alice")); err != nil {
		t.Errorf("user root must survive an empty-chatID removal: %v", err)
	}

	// A crafted chatID can't escape the user root.
	if err := j.RemoveChatScope("alice", "../bob"); !errors.Is(err, ErrInvalidChatID) {
		t.Errorf("RemoveChatScope(../bob) = %v, want ErrInvalidChatID", err)
	}
}

func TestJailHomeDirIsSiblingNotNestedInARepo(t *testing.T) {
	j := newTestJail(t)
	// Simulate a cloned repo living directly under the user's jail root, the
	// way git_clone's default `dir` (the repo name) does.
	repoDir, err := j.Resolve("alice", "", "myrepo")
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
		t.Fatalf("HomeDir %q is nested inside the cloned repo %q - must be a sibling", home, repoDir)
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
