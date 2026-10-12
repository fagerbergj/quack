// Package pgtest shares one postgres testcontainer per test binary and gives each caller its own database,
// so packages pay one container start instead of one per test.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// The container is never terminated here: testcontainers' reaper removes it when the test binary exits,
// as internal/memory's shared qdrant container relies on.
var (
	once     sync.Once
	baseURL  *url.URL
	admin    *sql.DB
	startErr error
	dbSeq    atomic.Int64
)

func start() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		startErr = err
		return
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		startErr = err
		return
	}
	if baseURL, startErr = url.Parse(dsn); startErr != nil {
		return
	}
	admin, startErr = sql.Open("pgx", dsn)
}

// DSN returns the URL of a fresh, empty database on the shared container, dropped when tb ends; it skips
// tb when Docker isn't reachable.
func DSN(tb testing.TB) string {
	tb.Helper()
	once.Do(start)
	if startErr != nil {
		tb.Skipf("docker unavailable, skipping postgres test: %v", startErr)
	}
	// A counter, not tb.Name(): subtest names carry slashes and can pass postgres' 63-byte identifier limit.
	name := fmt.Sprintf("t%d", dbSeq.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		tb.Fatalf("pgtest: create database: %v", err)
	}
	// FORCE ends the backends of handles tests never close, which would otherwise pile up toward max_connections.
	tb.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
			tb.Logf("pgtest: drop database %s: %v", name, err)
		}
	})
	u := *baseURL
	u.Path = "/" + name
	return u.String()
}
