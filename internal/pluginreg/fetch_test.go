package pluginreg

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run runs a git command against dir, failing the test on error.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newFixtureRepo makes a bare repo (the "remote") plus a work tree that
// pushes to it, both under t.TempDir(). Returns the bare repo path, used as
// the fetch target in place of github.com.
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)

	work := t.TempDir()
	run(t, work, "init", "--quiet", "--initial-branch=main")
	run(t, work, "config", "user.email", "test@example.com")
	run(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "skills"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "v1")
	run(t, work, "remote", "add", "origin", bare)
	run(t, work, "push", "--quiet", "origin", "main")
	run(t, work, "tag", "v1")
	run(t, work, "push", "--quiet", "origin", "v1")

	return bare
}

// commitAndPush adds one more commit on top of the work tree used by
// newFixtureRepo and pushes it, returning the new sha.
func commitAndPush(t *testing.T, work, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "skills"), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", msg)
	run(t, work, "push", "--quiet", "origin", "main")
	return strings.TrimSpace(run(t, work, "rev-parse", "HEAD"))
}

// withFixedRemote overrides remoteURL to resolve owner/repo to a fixed local
// path (the bare fixture repo), restored on cleanup.
func withFixedRemote(t *testing.T, url string) {
	t.Helper()
	prev := remoteURL
	remoteURL = func(owner, repo string) string { return url }
	t.Cleanup(func() { remoteURL = prev })
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
	if got.FetchedAt.IsZero() {
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
	second, err := reg.Fetch(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA != first.SHA {
		t.Fatalf("second fetch changed sha: %q -> %q", first.SHA, second.SHA)
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
	// not from remoteURL(), so simulate "unreachable" by removing the bare
	// repo itself rather than re-pointing remoteURL.
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	bad, err := reg.Fetch(context.Background(), good)
	if err != nil {
		t.Fatal(err)
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
