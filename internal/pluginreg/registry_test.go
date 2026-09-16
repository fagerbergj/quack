package pluginreg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPutOverwritesExisting(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()

	if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", SHA: "aaa"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", SHA: "bbb"}); err != nil {
		t.Fatal(err)
	}

	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SHA != "bbb" {
		t.Fatalf("List() = %+v, want one row with sha bbb", list)
	}
}

// TestPutRejectsNameCollisionAcrossDifferentEntries: two entries that share
// a registry name (e.g. two repos both named "widgets") must not silently
// overwrite each other's row.
func TestPutRejectsNameCollisionAcrossDifferentEntries(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()

	if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatal(err)
	}
	err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:glob/widgets", Owner: "glob", Repo: "widgets"})
	if err == nil {
		t.Fatal("expected an error putting a different entry under an already-registered name")
	}

	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Entry != "github:acme/widgets" {
		t.Fatalf("List() = %+v, want the original row untouched", list)
	}
}

// TestPutAllowsMovingPinOnSameRepo is the #1429 carry-over from PR #1436's
// review: moving a pin (github:o/r@v1 -> github:o/r@v2) on the SAME repo
// must not be treated as a name collision.
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

func TestDeleteMissingNameIsErrNotExist(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	err := reg.Delete(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected an error deleting a name with no row")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete missing name error = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestDeleteRemovesCloneAndRow(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()
	if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets"}); err != nil {
		t.Fatal(err)
	}
	cloneDir := CloneDir(root, "widgets")
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := reg.Delete(ctx, "widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "widgets")); !os.IsNotExist(err) {
		t.Fatalf("row/clone dir still present after Delete: err=%v", err)
	}
}

func TestListOrderIsSortedByName(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := reg.Put(ctx, Plugin{Name: name, Source: SourceGitHub, Entry: "github:acme/" + name}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(list) != len(want) {
		t.Fatalf("List() = %+v, want %d rows", list, len(want))
	}
	for i, w := range want {
		if list[i].Name != w {
			t.Fatalf("List()[%d].Name = %q, want %q (List is documented sorted by name)", i, list[i].Name, w)
		}
	}
}

func TestPutConcurrent(t *testing.T) {
	root := t.TempDir()
	reg := NewFSRegistry(root)
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", SHA: "sha"})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put failed: %v", err)
		}
	}
	// The row must still be one valid JSON object, not a torn write.
	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("List() after concurrent Put = %+v, want exactly one row", list)
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
