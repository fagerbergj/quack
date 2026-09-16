package pluginreg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// regBackend is one Registry implementation under test, plus root (clone
// dirs and, for the filesystem backend, entry.json rows live there).
type regBackend struct {
	name string
	open func(t *testing.T) (Registry, string)
}

func fsBackend() regBackend {
	return regBackend{
		name: "filesystem",
		open: func(t *testing.T) (Registry, string) {
			root := t.TempDir()
			return NewFSRegistry(root), root
		},
	}
}

func sqliteBackend() regBackend {
	return regBackend{
		name: "sqlite",
		open: func(t *testing.T) (Registry, string) {
			root := t.TempDir()
			db, err := OpenDB("sqlite", filepath.Join(t.TempDir(), "plugins.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			reg, err := NewDBRegistry(db, root)
			if err != nil {
				t.Fatalf("NewDBRegistry: %v", err)
			}
			return reg, root
		},
	}
}

// postgresBackend skips the whole subtree when Docker isn't reachable -
// same pattern internal/store's testcontainers tests use.
func postgresBackend(t *testing.T) regBackend {
	return regBackend{
		name: "postgres",
		open: func(t *testing.T) (Registry, string) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
				tcpostgres.WithDatabase("quack_pluginreg_test"),
				tcpostgres.WithUsername("quack"),
				tcpostgres.WithPassword("quack"),
				tcpostgres.BasicWaitStrategies(),
			)
			if err != nil {
				t.Skipf("docker unavailable, skipping postgres registry test: %v", err)
			}
			t.Cleanup(func() {
				if err := ctr.Terminate(context.Background()); err != nil {
					t.Logf("terminate postgres container: %v", err)
				}
			})
			dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
			if err != nil {
				t.Fatalf("connection string: %v", err)
			}
			db, err := OpenDB("postgres", dsn)
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			root := t.TempDir()
			reg, err := NewDBRegistry(db, root)
			if err != nil {
				t.Fatalf("NewDBRegistry: %v", err)
			}
			return reg, root
		},
	}
}

// assertPersistedSHA reads name's sha straight off storage (entry.json, or
// a raw SQL SELECT for the DB backends) - proving the on-disk/in-db shape.
func assertPersistedSHA(t *testing.T, reg Registry, root, name, want string) {
	t.Helper()
	switch rr := reg.(type) {
	case *FSRegistry:
		b, err := os.ReadFile(filepath.Join(root, name, "entry.json"))
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(b, &row); err != nil {
			t.Fatal(err)
		}
		got, _ := row["sha"].(string)
		if got != want {
			t.Fatalf("entry.json[sha] = %q, want %q", got, want)
		}
	case *DBRegistry:
		var got string
		if err := rr.db.Raw(`SELECT installed_sha FROM plugin_rows WHERE name = ?`, name).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("plugin_rows.installed_sha = %q, want %q", got, want)
		}
	default:
		t.Fatalf("unknown Registry type %T", reg)
	}
}

func TestRegistryBackends(t *testing.T) {
	backends := []regBackend{fsBackend(), sqliteBackend(), postgresBackend(t)}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Run("PutOverwritesExisting", func(t *testing.T) {
				reg, root := b.open(t)
				ctx := context.Background()
				if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets", SHA: "aaa"}); err != nil {
					t.Fatal(err)
				}
				if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets", SHA: "bbb"}); err != nil {
					t.Fatal(err)
				}
				list, err := reg.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 || list[0].SHA != "bbb" {
					t.Fatalf("List() = %+v, want one row with sha bbb", list)
				}
				assertPersistedSHA(t, reg, root, "widgets", "bbb")
			})

			t.Run("PutRejectsNameCollisionAcrossDifferentEntries", func(t *testing.T) {
				reg, _ := b.open(t)
				ctx := context.Background()
				if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets"}); err != nil {
					t.Fatal(err)
				}
				err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:glob/widgets", Owner: "glob", Repo: "widgets"})
				if !errors.Is(err, ErrNameCollision) {
					t.Fatalf("Put collision error = %v, want errors.Is(err, ErrNameCollision)", err)
				}
				list, err := reg.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 || list[0].Entry != "github:acme/widgets" {
					t.Fatalf("List() = %+v, want the original row untouched", list)
				}
			})

			t.Run("PutPreservesShaOnUnfetchedReput", func(t *testing.T) {
				reg, root := b.open(t)
				ctx := context.Background()
				now := time.Now().UTC()
				if err := reg.Put(ctx, Plugin{
					Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets",
					Owner: "acme", Repo: "widgets", SHA: "aaaa", FetchedAt: &now,
				}); err != nil {
					t.Fatal(err)
				}
				// Re-Put with the fresh, unfetched row FromEntry would build (no sha).
				if err := reg.Put(ctx, Plugin{
					Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets",
					Owner: "acme", Repo: "widgets",
				}); err != nil {
					t.Fatal(err)
				}
				list, err := reg.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 || list[0].SHA != "aaaa" || list[0].FetchedAt == nil {
					t.Fatalf("List() = %+v, want sha aaaa and fetched_at preserved", list)
				}
				assertPersistedSHA(t, reg, root, "widgets", "aaaa")
			})

			t.Run("PutConcurrent", func(t *testing.T) {
				reg, _ := b.open(t)
				ctx := context.Background()
				const n = 50
				var wg sync.WaitGroup
				errs := make(chan error, n)
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						errs <- reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets", SHA: "sha"})
					}()
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatalf("concurrent Put failed: %v", err)
					}
				}
				list, err := reg.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 {
					t.Fatalf("List() after concurrent Put = %+v, want exactly one row", list)
				}
			})

			t.Run("DeleteMissingNameIsErrNotExist", func(t *testing.T) {
				reg, _ := b.open(t)
				err := reg.Delete(context.Background(), "nope")
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Delete missing name error = %v, want it to wrap os.ErrNotExist", err)
				}
			})

			t.Run("DeleteRemovesClone", func(t *testing.T) {
				reg, root := b.open(t)
				ctx := context.Background()
				if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets"}); err != nil {
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
				if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
					t.Fatalf("clone dir still present after Delete: err=%v", err)
				}
			})

			t.Run("ListOrderIsSortedByName", func(t *testing.T) {
				reg, _ := b.open(t)
				ctx := context.Background()
				for _, name := range []string{"zeta", "alpha", "mid"} {
					if err := reg.Put(ctx, Plugin{Name: name, Source: SourceGitHub, Entry: "github:acme/" + name, Owner: "acme", Repo: name}); err != nil {
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
						t.Fatalf("List()[%d].Name = %q, want %q", i, list[i].Name, w)
					}
				}
			})

			t.Run("InvalidNamesRejected", func(t *testing.T) {
				reg, _ := b.open(t)
				err := reg.Put(context.Background(), Plugin{Name: "..", Source: SourceLocal, Entry: ".."})
				if !errors.Is(err, ErrInvalidName) {
					t.Fatalf("Put(..) error = %v, want errors.Is(err, ErrInvalidName)", err)
				}
				err = reg.Delete(context.Background(), "a/../../victim")
				if !errors.Is(err, ErrInvalidName) {
					t.Fatalf("Delete(a/../../victim) error = %v, want errors.Is(err, ErrInvalidName)", err)
				}
			})

			// PutAllowsMovingPinOnSameRepo is the #1429 carry-over: moving a
			// pin (github:o/r@v1 -> github:o/r@v2) on the SAME repo must not
			// be treated as a name collision, on every backend.
			t.Run("PutAllowsMovingPinOnSameRepo", func(t *testing.T) {
				reg, _ := b.open(t)
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
			})

			// PutSameIdentityWithEmptyOwnerRepoFallsBackToEntry: samePlugin
			// derives owner/repo from Entry when Owner/Repo are blank (#1430) -
			// a re-Put built that way (FromEntry's shape) must still match the
			// SAME repo's existing row, on every backend, not collide.
			t.Run("PutSameIdentityWithEmptyOwnerRepoFallsBackToEntry", func(t *testing.T) {
				reg, _ := b.open(t)
				ctx := context.Background()
				if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", Owner: "acme", Repo: "widgets", SHA: "aaa"}); err != nil {
					t.Fatal(err)
				}
				// No Owner/Repo set - samePlugin must resolve identity via
				// ParseEntry(Entry) instead of comparing blank fields.
				if err := reg.Put(ctx, Plugin{Name: "widgets", Source: SourceGitHub, Entry: "github:acme/widgets", SHA: "bbb"}); err != nil {
					t.Fatalf("re-Put with empty owner/repo (entry-derived identity) was rejected: %v", err)
				}
				list, err := reg.List(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(list) != 1 || list[0].SHA != "bbb" {
					t.Fatalf("List() = %+v, want one row with sha bbb", list)
				}
			})
		})
	}
}
