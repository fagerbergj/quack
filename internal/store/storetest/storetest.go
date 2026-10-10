// Package storetest holds test-only instrumentation for internal/store.
package storetest

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"
)

var seq atomic.Int64

// RecordQueries captures the SQL of every SELECT db issues until the test ends; the returned func
// snapshots them, oldest first. db shares its callbacks with the store's session service.
func RecordQueries(t testing.TB, db *gorm.DB) func() []string {
	t.Helper()
	var mu sync.Mutex
	var sqls []string
	name := fmt.Sprintf("storetest:record_sql:%d", seq.Add(1))
	if err := db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		mu.Lock()
		sqls = append(sqls, tx.Statement.SQL.String())
		mu.Unlock()
	}); err != nil {
		t.Fatalf("register query recorder: %v", err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(sqls)
	}
}
