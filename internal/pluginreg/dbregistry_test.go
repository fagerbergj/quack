package pluginreg

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestOpenDBHandlesStayIndependentAfterSequentialOpens: a shared
// *gorm.Config across OpenDB calls (gorm.DB embeds *Config) let a LATER
// open silently repoint an EARLIER handle's Dialector/ConnPool too.
func TestOpenDBHandlesStayIndependentAfterSequentialOpens(t *testing.T) {
	ctx := context.Background()

	sqliteDB, err := OpenDB("sqlite", filepath.Join(t.TempDir(), "plugins.db"))
	if err != nil {
		t.Fatalf("OpenDB sqlite: %v", err)
	}
	sqliteReg, err := NewDBRegistry(sqliteDB, t.TempDir())
	if err != nil {
		t.Fatalf("NewDBRegistry sqlite: %v", err)
	}
	if err := sqliteReg.Put(ctx, Plugin{Name: "a", Source: SourceGitHub, Entry: "github:acme/a", Owner: "acme", Repo: "a"}); err != nil {
		t.Fatal(err)
	}

	pgCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ctr, err := tcpostgres.Run(pgCtx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_opendb_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping OpenDB independence test: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(pgCtx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pgDB, err := OpenDB("postgres", dsn)
	if err != nil {
		t.Fatalf("OpenDB postgres: %v", err)
	}
	pgReg, err := NewDBRegistry(pgDB, t.TempDir())
	if err != nil {
		t.Fatalf("NewDBRegistry postgres: %v", err)
	}
	if err := pgReg.Put(ctx, Plugin{Name: "b", Source: SourceGitHub, Entry: "github:acme/b", Owner: "acme", Repo: "b"}); err != nil {
		t.Fatal(err)
	}

	// The bug, directly: opening postgres above must NOT repoint the
	// earlier sqlite handle's own Dialector.
	if got := sqliteDB.Dialector.Name(); got != "sqlite" {
		t.Fatalf("sqliteDB.Dialector.Name() after opening postgres too = %q, want sqlite (a shared gorm.Config repointed the earlier handle)", got)
	}

	sqliteList, err := sqliteReg.List(ctx)
	if err != nil {
		t.Fatalf("sqliteReg.List() after opening postgres too: %v", err)
	}
	if len(sqliteList) != 1 || sqliteList[0].Name != "a" {
		t.Fatalf("sqliteReg.List() = %+v, want [a]", sqliteList)
	}
	pgList, err := pgReg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pgList) != 1 || pgList[0].Name != "b" {
		t.Fatalf("pgReg.List() = %+v, want [b]", pgList)
	}
}

// putConcurrentCollision runs Put(name, ownerA/repoA) and Put(name,
// ownerB/repoB) concurrently through two SEPARATE handles (two quack
// nodes on one DB) and reports how many succeeded.
func putConcurrentCollision(t *testing.T, regA, regB Registry, name string) (successes int, collisions int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = regA.Put(context.Background(), Plugin{Name: name, Source: SourceGitHub, Entry: "github:acme/" + name, Owner: "acme", Repo: name})
	}()
	go func() {
		defer wg.Done()
		errs[1] = regB.Put(context.Background(), Plugin{Name: name, Source: SourceGitHub, Entry: "github:other/" + name, Owner: "other", Repo: name})
	}()
	wg.Wait()
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrNameCollision):
			collisions++
		default:
			t.Fatalf("Put error = %v, want nil or ErrNameCollision", err)
		}
	}
	return successes, collisions
}

// TestDBRegistryPutRejectsConcurrentInsertCollision_Sqlite: two SEPARATE
// handles (two sqlite connections to one file, serialised by WAL) racing
// an insert of different identities under one new name must not both pass.
func TestDBRegistryPutRejectsConcurrentInsertCollision_Sqlite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins.db")
	root := t.TempDir()
	dbA, err := OpenDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	regA, err := NewDBRegistry(dbA, root)
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := OpenDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	regB, err := NewDBRegistry(dbB, root)
	if err != nil {
		t.Fatal(err)
	}

	successes, collisions := putConcurrentCollision(t, regA, regB, "widgets")
	if successes != 1 || collisions != 1 {
		t.Fatalf("concurrent insert-insert race = %d successes, %d collisions, want exactly 1 and 1 (last-writer-wins with no error otherwise)", successes, collisions)
	}
}

// TestDBRegistryPutRejectsConcurrentInsertCollision_Postgres is the same
// race as the sqlite case, against real postgres (skipped without Docker).
func TestDBRegistryPutRejectsConcurrentInsertCollision_Postgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_put_race_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres Put-race test: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dbA, err := OpenDB("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	regA, err := NewDBRegistry(dbA, root)
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := OpenDB("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	regB, err := NewDBRegistry(dbB, root)
	if err != nil {
		t.Fatal(err)
	}

	successes, collisions := putConcurrentCollision(t, regA, regB, "widgets")
	if successes != 1 || collisions != 1 {
		t.Fatalf("concurrent insert-insert race = %d successes, %d collisions, want exactly 1 and 1 (last-writer-wins with no error otherwise)", successes, collisions)
	}
}
