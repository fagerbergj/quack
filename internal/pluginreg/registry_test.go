package pluginreg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Moving a pin (github:o/r@v1 -> @v2) on the same repo is an update, not a name collision.
func TestPutAllowsMovingPinOnSameRepo(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()

	if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets@v1", Owner: "acme", Repo: "widgets", Ref: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets@v2", Owner: "acme", Repo: "widgets", Ref: "v2"}); err != nil {
		t.Fatalf("moving the pin on the same repo was rejected: %v", err)
	}

	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Ref != "v2" {
		t.Fatalf("List() = %+v, want one row pinned at v2", list)
	}
}

func TestEntryJSONFieldNamesLiteral(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()
	sha := "1234567890123456789012345678901234567890"
	if err := reg.Put(ctx, Plugin{
		Entry: "github:acme/widgets@v1#skills", Source: "github", Name: "widgets",
		Owner: "acme", Repo: "widgets", Ref: "v1", Path: "skills", SHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(root, "widgets", "entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"entry": "github:acme/widgets@v1#skills", "source": "github", "name": "widgets",
		"owner": "acme", "repo": "widgets", "ref": "v1", "path": "skills", "sha": sha,
	}
	for k, v := range want {
		if row[k] != v {
			t.Fatalf("entry.json[%q] = %v, want %v", k, row[k], v)
		}
	}
	if _, ok := row["fetched_at"]; ok {
		t.Fatalf("entry.json has fetched_at with no fetch: %v", row["fetched_at"])
	}
	if _, ok := row["error"]; ok {
		t.Fatalf("entry.json has error with no failure: %v", row["error"])
	}
}

// Two github rows that both fail to resolve an owner/repo are never the same plugin merely because both
// are blank.
func TestSameIdentityEmptyBothIsFalse(t *testing.T) {
	a := Plugin{Source: SourceGitHub, Entry: "not-a-github-entry"}
	b := Plugin{Source: SourceGitHub, Entry: "also-not-one"}
	if SameIdentity(a, b) {
		t.Fatalf("SameIdentity(%+v, %+v) = true, want false (both unresolvable)", a, b)
	}
}

// TestRootGitHubWithAndWithoutPath covers Plugin.Root's github branch: no
// #path serves the clone root; a non-escaping #path serves the joined dir.
func TestRootGitHubWithAndWithoutPath(t *testing.T) {
	p := Plugin{Name: "widgets", Source: SourceGitHub}
	if got, want := p.Root("/reg"), CloneDir("/reg", "widgets"); got != want {
		t.Errorf("Root() with no path = %q, want %q", got, want)
	}
	p.Path = "skills"
	if got, want := p.Root("/reg"), filepath.Join(CloneDir("/reg", "widgets"), "skills"); got != want {
		t.Errorf("Root() with path = %q, want %q", got, want)
	}
}

// A Path escaping the clone (rows are trusted off disk) falls back to the clone root; also covers
// containedPath's own escape check.
func TestRootGitHubEscapingPathFallsBackToCloneDir(t *testing.T) {
	p := Plugin{Name: "widgets", Source: SourceGitHub, Path: "../../etc"}
	if got, want := p.Root("/reg"), CloneDir("/reg", "widgets"); got != want {
		t.Errorf("Root() with an escaping path = %q, want the clone root %q", got, want)
	}
}

// TestEmbeddedQuackPlugin: the in-memory embedded row's fixed shape.
func TestEmbeddedQuackPlugin(t *testing.T) {
	p := EmbeddedQuackPlugin()
	if p.Name != EmbeddedQuackPluginName || p.Source != SourceEmbedded {
		t.Errorf("EmbeddedQuackPlugin() = %+v, want Name %q Source %q", p, EmbeddedQuackPluginName, SourceEmbedded)
	}
}

// Seed order wins for rows it names; rows not in seed sort after in List's order; an unparsable seed entry
// is skipped, not fatal.
func TestOrderBySeedOrdersBySeedThenAppendsRest(t *testing.T) {
	rows := []Plugin{
		{Name: "alpha"}, {Name: "beta"}, {Name: "extra"},
	}
	seed := []string{"github:acme/beta", "not-github-but-fine-as-a-local-root", "github:bad entry with spaces"}
	// "not-github-but-fine-as-a-local-root" resolves to a local entry named
	// by its own base - not one of the rows above, so it contributes nothing.
	got := OrderBySeed(seed, rows)
	names := make([]string, len(got))
	for i, p := range got {
		names[i] = p.Name
	}
	want := []string{"beta", "alpha", "extra"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("OrderBySeed names = %v, want %v", names, want)
	}
}

// TestGithubIdentityUnresolvableEntry: an Entry ParseEntry itself rejects
// (not merely "no owner/repo") falls through to the blank identity.
func TestGithubIdentityUnresolvableEntry(t *testing.T) {
	owner, repo := githubIdentity(Plugin{Source: SourceGitHub, Entry: ""})
	if owner != "" || repo != "" {
		t.Errorf("githubIdentity(empty entry) = (%q, %q), want (\"\", \"\")", owner, repo)
	}
}

// MkdirAll fails when a regular file occupies the row's directory path (a real on-disk corruption case).
func TestPutFailsWhenNameCollidesWithAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "widgets"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewFSRegistry(root)
	err := reg.Put(context.Background(), Plugin{Name: "widgets", Source: SourceLocal, Entry: "widgets"})
	if err == nil {
		t.Fatal("Put into a name blocked by a file = nil, want an error")
	}
}

// TestPutFailsWhenDirIsReadOnly: CreateTemp fails when the row's directory
// exists but isn't writable (permission-denied, not a mock).
func TestPutFailsWhenDirIsReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "widgets")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	reg := NewFSRegistry(root)
	err := reg.Put(context.Background(), Plugin{Name: "widgets", Source: SourceLocal, Entry: "widgets"})
	if err == nil {
		t.Fatal("Put into a read-only directory = nil, want an error")
	}
}

// A Stat failure other than not-exist (permission denied) propagates as itself, not as Delete's wrapped
// os.ErrNotExist.
func TestDeleteStatGenericErrorIsNotNotFound(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	reg := NewFSRegistry(root)
	if err := reg.Put(context.Background(), Plugin{Name: "widgets", Source: SourceLocal, Entry: "widgets"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	err := reg.Delete(context.Background(), "widgets")
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete() with an unstatable parent = %v, want a non-ErrNotExist error", err)
	}
}
