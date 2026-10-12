// Package sqlitedsn builds a sqlite DSN with WAL/busy-timeout/FK pragmas, a leaf package so store and
// pluginreg can share it without an import cycle.
package sqlitedsn

import (
	"strings"
	"testing"
)

// Build enables WAL, busy timeout and FK enforcement (SQLite defaults foreign_keys off per connection).
// A URL that already has query params is left untouched.
func Build(url string) string {
	// Test binaries skip fsync: it cuts store.New about 5x, and a test DB never has to survive a power loss.
	return build(url, testing.Testing())
}

func build(url string, noSync bool) string {
	if strings.Contains(url, "?") {
		return url
	}
	dsn := url + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if noSync {
		dsn += "&_pragma=synchronous(OFF)"
	}
	return dsn
}
