package rest

import (
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/pgtest"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPGLedgerStore returns a ledger.PGStore on a real Postgres database;
// skips when Docker isn't reachable.
func newTestPGLedgerStore(t *testing.T) *ledger.PGStore {
	t.Helper()
	db, err := gorm.Open(postgres.Open(pgtest.DSN(t)), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	store, err := ledger.NewPGStore(db)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	return store
}
