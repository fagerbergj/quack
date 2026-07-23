package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepo fakes a clone: a .git dir with a config naming an origin remote.
func writeRepo(t *testing.T, dir, originURL string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n"
	if originURL != "" {
		cfg += "[remote \"origin\"]\n\turl = " + originURL + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRepoKey: the memory repo bucket's key is DERIVED from the chat's clone - the
// same repo cloned over ssh or https is one bucket, and an ambiguous or repo-less
// scope yields "" (fall back to the role bucket, never guess).
func TestRepoKey(t *testing.T) {
	root := t.TempDir()
	j, err := NewJail(root)
	if err != nil {
		t.Fatalf("NewJail: %v", err)
	}

	// No repo in the chat scope yet → no key.
	if got := j.RepoKey("u1", "c1"); got != "" {
		t.Fatalf("empty scope RepoKey = %q, want \"\"", got)
	}

	scope, err := j.Resolve("u1", "c1", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepo(t, filepath.Join(scope, "games"), "git@github.com:Acme/Games.git")
	if got, want := j.RepoKey("u1", "c1"), "github.com/acme/games"; got != want {
		t.Fatalf("RepoKey = %q, want %q", got, want)
	}

	// The https clone of the same repo is the SAME bucket (another chat, same repo).
	scope2, _ := j.Resolve("u1", "c2", "")
	if err := os.MkdirAll(scope2, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepo(t, filepath.Join(scope2, "games"), "https://x-access-token:tok@github.com/acme/games.git")
	if got, want := j.RepoKey("u1", "c2"), "github.com/acme/games"; got != want {
		t.Fatalf("https clone RepoKey = %q, want %q (one repo, one bucket)", got, want)
	}

	// Two repos in one scope is ambiguous - no key rather than a guess.
	writeRepo(t, filepath.Join(scope, "other"), "https://github.com/acme/other.git")
	if got := j.RepoKey("u1", "c1"); got != "" {
		t.Fatalf("ambiguous scope RepoKey = %q, want \"\" (don't guess)", got)
	}

	// A local-only repo (no origin) has no shareable identity.
	scope3, _ := j.Resolve("u1", "c3", "")
	if err := os.MkdirAll(scope3, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepo(t, filepath.Join(scope3, "local"), "")
	if got := j.RepoKey("u1", "c3"); got != "" {
		t.Fatalf("origin-less repo RepoKey = %q, want \"\"", got)
	}
}
