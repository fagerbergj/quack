package rest

import (
	"context"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPGLedgerStore starts a real Postgres container and returns a
// ledger.PGStore backed by it - mirrors internal/ledger's own container
// test and internal/store's artifact_largeobject_test.go. Skips (not
// fails) when Docker isn't reachable.
func newTestPGLedgerStore(t *testing.T) *ledger.PGStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_rest_ledger_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping postgres ledger integration test: %v", err)
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
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	store, err := ledger.NewPGStore(db)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	return store
}
