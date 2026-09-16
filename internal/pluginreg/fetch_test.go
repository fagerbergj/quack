package pluginreg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
)

// run/newFixtureRepo/commitAndPush wrap pluginregtest, the implementation
// shared with internal/server/rest's plugin handler tests (#1430).
func run(t *testing.T, dir string, args ...string) string {
	return pluginregtest.RunGit(t, dir, args...)
}

func newFixtureRepo(t *testing.T) string {
	bare, _ := pluginregtest.NewFixtureRepo(t)
	return bare
}

func commitAndPush(t *testing.T, work, msg string) string {
	return pluginregtest.CommitAndPush(t, work, msg)
}

// withFixedRemote overrides RemoteURL directly (same package - pluginregtest
// can't import pluginreg, or an internal test file importing it would cycle).
func withFixedRemote(t *testing.T, url string) {
	t.Helper()
	prev := RemoteURL
	RemoteURL = func(owner, repo string) string { return url }
	t.Cleanup(func() { RemoteURL = prev })
}

func TestFetchClonesAndRecordsSHA(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)

	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, err := ParseEntry("github:acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	got, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Error != "" {
		t.Fatalf("Fetch recorded an error: %s", got.Error)
	}
	wantSHA := strings.TrimSpace(run(t, bare, "rev-parse", "main"))
	if got.SHA != wantSHA {
		t.Fatalf("SHA = %q, want %q", got.SHA, wantSHA)
	}
	if got.FetchedAt == nil {
		t.Fatal("FetchedAt not set")
	}

	// On-disk shape: literal paths and entry.json field names.
	cloneDir := filepath.Join(root, "widgets", "repo")
	if st, err := os.Stat(cloneDir); err != nil || !st.IsDir() {
		t.Fatalf("clone dir %s missing: %v", cloneDir, err)
	}
	rowFile := filepath.Join(root, "widgets", "entry.json")
	b, err := os.ReadFile(rowFile)
	if err != nil {
		t.Fatalf("read %s: %v", rowFile, err)
	}
	var row map[string]any
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	if row["entry"] != "github:acme/widgets" {
		t.Fatalf("entry.json[entry] = %v, want github:acme/widgets", row["entry"])
	}
	if row["source"] != "github" {
		t.Fatalf("entry.json[source] = %v, want github", row["source"])
	}
	if row["name"] != "widgets" {
		t.Fatalf("entry.json[name] = %v, want widgets", row["name"])
	}
	if row["sha"] != wantSHA {
		t.Fatalf("entry.json[sha] = %v, want %v", row["sha"], wantSHA)
	}
}

func TestFetchSecondCallIsNoOp(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets")
	p := FromEntry(e)

	first, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	// An untracked marker inside the clone only survives a `git fetch` on the
	// existing tree - a reclone (rm -rf + clone) would wipe it.
	marker := filepath.Join(root, "widgets", "repo", "untracked-marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := reg.Fetch(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA != first.SHA {
		t.Fatalf("second fetch changed sha: %q -> %q", first.SHA, second.SHA)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("second fetch reset the clone (marker gone): %v", err)
	}
}

func TestFetchMovedTagUpdates(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets@v1")
	p := FromEntry(e)

	first, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	firstSHA := first.SHA

	// Move the tag to a new commit in a fresh work tree cloned from bare.
	work2 := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work2)
	run(t, work2, "config", "user.email", "test@example.com")
	run(t, work2, "config", "user.name", "test")
	newSHA := commitAndPush(t, work2, "v2")
	run(t, work2, "tag", "-f", "v1")
	run(t, work2, "push", "--quiet", "--force", "origin", "v1")

	second, err := reg.Fetch(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA != newSHA {
		t.Fatalf("moved tag: SHA = %q, want %q (was %q)", second.SHA, newSHA, firstSHA)
	}
}

func TestFetchRemoteUnreachableKeepsOldSHA(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets")
	p := FromEntry(e)

	good, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	oldSHA := good.SHA

	// Existing clones fetch from the origin remote already recorded in .git,
	// not from RemoteURL(), so simulate "unreachable" by removing the bare
	// repo itself rather than re-pointing RemoteURL.
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	bad, err := reg.Fetch(context.Background(), good)
	if err == nil {
		t.Fatal("expected Fetch to return the fetch error, not swallow it (issue 10)")
	}
	if bad.SHA != oldSHA {
		t.Fatalf("SHA changed on failed fetch: %q -> %q", oldSHA, bad.SHA)
	}
	if bad.Error == "" {
		t.Fatal("expected an error recorded on the row")
	}

	stored, err := reg.readRow("widgets")
	if err != nil {
		t.Fatal(err)
	}
	if stored.SHA != oldSHA {
		t.Fatalf("persisted SHA changed: %q -> %q", oldSHA, stored.SHA)
	}
	if stored.Error == "" {
		t.Fatal("persisted row has no error")
	}
}

func TestCheckUpdate(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets")
	p := FromEntry(e)

	fetched, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	behind, _, err := reg.CheckUpdate(context.Background(), fetched)
	if err != nil {
		t.Fatal(err)
	}
	if behind {
		t.Fatal("freshly fetched plugin reported behind")
	}

	work2 := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work2)
	run(t, work2, "config", "user.email", "test@example.com")
	run(t, work2, "config", "user.name", "test")
	newSHA := commitAndPush(t, work2, "v2")

	behind, remoteSHA, err := reg.CheckUpdate(context.Background(), fetched)
	if err != nil {
		t.Fatal(err)
	}
	if !behind {
		t.Fatal("expected behind after a new push")
	}
	if remoteSHA != newSHA {
		t.Fatalf("remoteSHA = %q, want %q", remoteSHA, newSHA)
	}
}

func TestCheckUpdatePinnedSHANeverBehind(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	sha := strings.TrimSpace(run(t, bare, "rev-parse", "main"))
	e, err := ParseEntry("github:acme/widgets@" + sha)
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)
	p.SHA = sha

	behind, _, err := reg.CheckUpdate(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if behind {
		t.Fatal("a pinned sha must never be behind")
	}
}

func TestFetchPinnedNonDefaultBranch(t *testing.T) {
	bare := newFixtureRepo(t)
	work2 := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work2)
	run(t, work2, "config", "user.email", "test@example.com")
	run(t, work2, "config", "user.name", "test")
	run(t, work2, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work2, "skills"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work2, "add", ".")
	run(t, work2, "commit", "--quiet", "-m", "feature-only")
	run(t, work2, "push", "--quiet", "origin", "feature")
	featureSHA := strings.TrimSpace(run(t, work2, "rev-parse", "HEAD"))

	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	// "feature" was never the checked-out branch of the initial clone, so
	// only a remote-tracking ref exists for it - `checkout --detach feature`
	// used to fail outright for exactly this shape.
	e, err := ParseEntry("github:acme/widgets@feature")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	got, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Fatalf("pinning a branch not checked out by the initial clone failed: %s", got.Error)
	}
	if got.SHA != featureSHA {
		t.Fatalf("SHA = %q, want %q", got.SHA, featureSHA)
	}
}

func TestFetchPinnedBranchAdvances(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, err := ParseEntry("github:acme/widgets@main")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	first, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if first.Error != "" {
		t.Fatalf("pinned branch fetch failed: %s", first.Error)
	}

	work2 := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work2)
	run(t, work2, "config", "user.email", "test@example.com")
	run(t, work2, "config", "user.name", "test")
	newSHA := commitAndPush(t, work2, "v2")

	behind, remoteSHA, err := reg.CheckUpdate(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if !behind || remoteSHA != newSHA {
		t.Fatalf("CheckUpdate = behind=%v remoteSHA=%q, want behind=true remoteSHA=%q", behind, remoteSHA, newSHA)
	}

	second, err := reg.Fetch(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA != newSHA {
		t.Fatalf("pinned branch did not advance: SHA = %q, want %q", second.SHA, newSHA)
	}
	if behind, _, err := reg.CheckUpdate(context.Background(), second); err != nil {
		t.Fatal(err)
	} else if behind {
		t.Fatal("after refetch, pinned branch still reports behind")
	}
}

func TestCheckUpdateAnnotatedTagPeels(t *testing.T) {
	bare := newFixtureRepo(t)
	work := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work)
	run(t, work, "config", "user.email", "test@example.com")
	run(t, work, "config", "user.name", "test")
	run(t, work, "tag", "-a", "v2", "-m", "v2 annotated")
	run(t, work, "push", "--quiet", "origin", "v2")
	commitSHA := strings.TrimSpace(run(t, work, "rev-parse", "v2^{commit}"))

	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, err := ParseEntry("github:acme/widgets@v2")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	if _, remoteSHA, err := reg.CheckUpdate(context.Background(), p); err != nil {
		t.Fatal(err)
	} else if remoteSHA != commitSHA {
		t.Fatalf("CheckUpdate annotated tag sha = %q, want the peeled commit %q", remoteSHA, commitSHA)
	}

	fetched, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.SHA != commitSHA {
		t.Fatalf("Fetch annotated tag sha = %q, want %q", fetched.SHA, commitSHA)
	}
}

// TestCheckUpdateBranchWinsOverSameNamedTag: a branch and a tag can share a
// name; CheckUpdate must agree with resolveSHA's precedence (branch first),
// or Fetch checks out one commit while CheckUpdate reports a different one
// as "current" forever.
func TestCheckUpdateBranchWinsOverSameNamedTag(t *testing.T) {
	bare := newFixtureRepo(t)
	work := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work)
	run(t, work, "config", "user.email", "test@example.com")
	run(t, work, "config", "user.name", "test")

	// A lightweight tag "dup" on the initial commit ...
	run(t, work, "tag", "dup")
	run(t, work, "push", "--quiet", "origin", "refs/tags/dup")
	// ... and a branch also named "dup", on a different commit. Both refspecs
	// are qualified since "dup" alone is now ambiguous between the two.
	run(t, work, "checkout", "--quiet", "-b", "dup")
	if err := os.WriteFile(filepath.Join(work, "skills"), []byte("branch"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "branch dup")
	run(t, work, "push", "--quiet", "origin", "refs/heads/dup:refs/heads/dup")
	branchSHA := strings.TrimSpace(run(t, work, "rev-parse", "HEAD"))

	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, err := ParseEntry("github:acme/widgets@dup")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	if _, remoteSHA, err := reg.CheckUpdate(context.Background(), p); err != nil {
		t.Fatal(err)
	} else if remoteSHA != branchSHA {
		t.Fatalf("CheckUpdate = %q, want the branch's sha %q (branch must win over the same-named tag)", remoteSHA, branchSHA)
	}

	fetched, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.SHA != branchSHA {
		t.Fatalf("Fetch = %q, want the branch's sha %q", fetched.SHA, branchSHA)
	}
}

func TestCheckUpdateShortHexRefIsNotPinned(t *testing.T) {
	bare := newFixtureRepo(t)
	work2 := t.TempDir()
	run(t, "", "clone", "--quiet", bare, work2)
	run(t, work2, "config", "user.email", "test@example.com")
	run(t, work2, "config", "user.name", "test")
	run(t, work2, "checkout", "--quiet", "-b", "deadbeef")
	if err := os.WriteFile(filepath.Join(work2, "skills"), []byte("branch"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work2, "add", ".")
	run(t, work2, "commit", "--quiet", "-m", "branch literally named deadbeef")
	run(t, work2, "push", "--quiet", "origin", "deadbeef")
	branchSHA := strings.TrimSpace(run(t, work2, "rev-parse", "HEAD"))

	withFixedRemote(t, bare)
	e, err := ParseEntry("github:acme/widgets@deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	behind, remoteSHA, err := CheckUpdate(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if remoteSHA != branchSHA {
		t.Fatalf("remoteSHA = %q, want the branch tip %q - a short hex ref must resolve via ls-remote, not short-circuit as a pinned sha", remoteSHA, branchSHA)
	}
	if !behind {
		t.Fatal("expected behind: p.SHA is empty, remote has a commit")
	}
}

func TestFetchReclonesUnstartedGitDir(t *testing.T) {
	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets")
	p := FromEntry(e)

	// Simulate a killed clone: the clone dir exists but was never `git init`ed.
	dir := filepath.Join(root, "widgets", "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Fatalf("Fetch failed on a corrupt clone dir: %s", got.Error)
	}
	wantSHA := strings.TrimSpace(run(t, bare, "rev-parse", "main"))
	if got.SHA != wantSHA {
		t.Fatalf("SHA = %q, want %q", got.SHA, wantSHA)
	}
}

func TestFetchRepointsOriginWhenEntryMoves(t *testing.T) {
	bareA := newFixtureRepo(t)
	bareB := newFixtureRepo(t)

	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets")
	p := FromEntry(e)

	withFixedRemote(t, bareA)
	first, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	wantA := strings.TrimSpace(run(t, bareA, "rev-parse", "main"))
	if first.SHA != wantA {
		t.Fatalf("SHA = %q, want %q", first.SHA, wantA)
	}

	// Re-point the same registry name at a different repo, as if the entry's
	// owner/repo changed under an unchanged plugin name.
	withFixedRemote(t, bareB)
	second, err := reg.Fetch(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	wantB := strings.TrimSpace(run(t, bareB, "rev-parse", "main"))
	if second.SHA != wantB {
		t.Fatalf("after repoint, SHA = %q, want %q (was %q)", second.SHA, wantB, wantA)
	}
}

func TestFetchDoesNotLeakTokenOnFailure(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "super-secret-token")
	withFixedRemote(t, filepath.Join(t.TempDir(), "nonexistent.git"))
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, _ := ParseEntry("github:acme/widgets")
	p := FromEntry(e)

	got, err := reg.Fetch(context.Background(), p)
	if err == nil {
		t.Fatal("expected the fetch against a nonexistent remote to fail")
	}
	if got.Error == "" {
		t.Fatal("expected an error recorded on the row")
	}
	if strings.Contains(got.Error, "super-secret-token") {
		t.Fatalf("row error leaks the token: %s", got.Error)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("returned error leaks the token: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(root, "widgets", "entry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "super-secret-token") {
		t.Fatalf("entry.json leaks the token: %s", b)
	}
}

// TestFetchDoesNotEscapeToOuterRepo: a killed clone's dir, nested inside the
// user's own workspace checkout, must never be mistaken for that outer repo
// (rev-parse --git-dir walks up when dir has no .git of its own).
func TestFetchDoesNotEscapeToOuterRepo(t *testing.T) {
	outer := t.TempDir()
	run(t, "", "init", "--quiet", "--initial-branch=main", outer)
	run(t, outer, "config", "user.email", "test@example.com")
	run(t, outer, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(outer, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, outer, "add", ".")
	run(t, outer, "commit", "--quiet", "-m", "init")
	run(t, outer, "remote", "add", "origin", "https://example.invalid/outer.git")
	outerHEAD := strings.TrimSpace(run(t, outer, "rev-parse", "HEAD"))

	// The registry root lives inside the outer repo's working tree, and the
	// clone dir is pre-seeded as a plain (non-repo) dir - a killed clone.
	root := filepath.Join(outer, ".quack", "plugins")
	dir := CloneDir(root, "widgets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	bare := newFixtureRepo(t)
	withFixedRemote(t, bare)
	e, err := ParseEntry("github:acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)

	got, err := Fetch(context.Background(), root, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Fatalf("Fetch failed: %s", got.Error)
	}
	wantSHA := strings.TrimSpace(run(t, bare, "rev-parse", "main"))
	if got.SHA != wantSHA {
		t.Fatalf("SHA = %q, want %q", got.SHA, wantSHA)
	}

	if url := strings.TrimSpace(run(t, outer, "remote", "get-url", "origin")); url != "https://example.invalid/outer.git" {
		t.Fatalf("outer repo's origin url was changed: %q", url)
	}
	if head := strings.TrimSpace(run(t, outer, "rev-parse", "HEAD")); head != outerHEAD {
		t.Fatalf("outer repo's HEAD moved: %q -> %q", outerHEAD, head)
	}
	if !isGitRepo(context.Background(), dir) {
		t.Fatal("clone dir was not recognized as its own git repo after Fetch")
	}
}

func TestLocalEntryFetchIsNoClone(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	e, err := ParseEntry(".agents/vendor/dotagents")
	if err != nil {
		t.Fatal(err)
	}
	p := FromEntry(e)
	got, err := reg.Fetch(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA != "" {
		t.Fatalf("local entry recorded a sha: %q", got.SHA)
	}
	if got.Root(root) != ".agents/vendor/dotagents" {
		t.Fatalf("local Root() = %q, want the path itself", got.Root(root))
	}
}
