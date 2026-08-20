package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatDirName(t *testing.T) {
	tests := []struct {
		id       string
		wantSame bool // clean ids map to themselves
	}{
		{"7b8a4c2e-uuid-style", true},
		{"chat_1.2", true},
		{"ext:github:github-fagerbergj-quack-965", false},
		{"a b", false}, // space
		{"a|b", false},
		{"a\\b", false},
	}
	for _, tc := range tests {
		got := ChatDirName(tc.id)
		if tc.wantSame != (got == tc.id) {
			t.Errorf("ChatDirName(%q) = %q, wantSame=%v", tc.id, got, tc.wantSame)
		}
		if strings.ContainsAny(got, ": |\\/") {
			t.Errorf("ChatDirName(%q) = %q still hostile", tc.id, got)
		}
		if got != ChatDirName(tc.id) {
			t.Errorf("ChatDirName(%q) not deterministic", tc.id)
		}
		// Sanitized names are fixed points, so GC can pass dir names back through scopeRoot.
		if again := ChatDirName(got); again != got {
			t.Errorf("ChatDirName(%q) not idempotent: %q -> %q", tc.id, got, again)
		}
	}
	// Hash suffix disambiguates ids that sanitize to the same string.
	if ChatDirName("ext:a:b") == ChatDirName("ext-a-b") || ChatDirName("ext:a:b") == ChatDirName("ext:a-b") {
		t.Error("distinct ids collided after sanitization")
	}
}

func TestScopeRootLegacyColonDirReuse(t *testing.T) {
	j, err := NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const user, chat = "u1", "ext:github:repo-965"

	// New chat: sanitized, colon-free dir.
	dir, err := j.EnsureDir(user, chat, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filepath.Base(dir), ":") {
		t.Fatalf("new chat dir %q contains colon", dir)
	}
	if got, err := j.Resolve(user, chat, ""); err != nil || got != dir {
		t.Fatalf("Resolve = %q, %v; want %q", got, err, dir)
	}

	// Existing prod layout: raw colon-named dir wins over sanitized.
	j2, err := NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userRoot, _ := j2.UserRoot(user)
	legacy := filepath.Join(userRoot, chat)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := j2.Resolve(user, chat, ""); err != nil || got != legacy {
		t.Fatalf("Resolve = %q, %v; want legacy %q", got, err, legacy)
	}
	// GC passes the on-disk name back; it must resolve to the same dir.
	if err := j2.RemoveChatScope(user, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy dir not removed: %v", err)
	}
}
